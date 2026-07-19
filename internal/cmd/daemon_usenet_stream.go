package cmd

import (
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/engine"
	"github.com/Unarr-app/unarr-cli/internal/ui"
)

// Usenet direct-play cold-buffer warm-up budget. Head covers the MKV/MP4 header
// (EBML head, Tracks, first frames); tail covers the index that sits at the end of
// a non-faststart container (mkv Cues/SeekHead, last ~128 KB) that VLC reads before
// it paints. Warming both into the upstream server cache makes the FIRST open play.
const (
	usenetPrewarmHeadBytes = 4 << 20 // 4 MiB
	usenetPrewarmTailBytes = 2 << 20 // 2 MiB
	usenetPrewarmTimeout   = 8 * time.Second
)

// prewarmUsenetHeadTail pre-fetches the video's first headBytes and last tailBytes
// THROUGH provider.NewFileReader — the exact reader path /stream serves — so the
// same NNTP articles are warmed in the upstream Usenet server cache (and the NNTP
// connection pool) before the player's first open. Head and tail are fetched
// CONCURRENTLY (two readers over the shared pool) to minimise added TTFB. It is
// best-effort and bounded by BOTH the caller's ctx (a per-task cancel aborts it)
// and timeout: on a slow/again NNTP it returns what it managed and the caller
// reports ready anyway — warming must never become a hang. Returns bytes actually
// read for head and tail plus the wall-clock duration.
func prewarmUsenetHeadTail(ctx context.Context, provider engine.FileProvider, headBytes, tailBytes int64, timeout time.Duration) (headRead, tailRead int64, dur time.Duration) {
	start := time.Now()
	if provider == nil {
		return 0, 0, 0
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	size := provider.FileSize()
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		headRead = warmRange(wctx, provider, 0, headBytes, size)
	}()

	if tailBytes > 0 && size > tailBytes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tailRead = warmRange(wctx, provider, size-tailBytes, tailBytes, size)
		}()
	}

	wg.Wait()
	return headRead, tailRead, time.Since(start)
}

// warmRange opens a fresh provider reader, seeks to offset, and drains up to n
// bytes into io.Discard so those articles are fetched + cached upstream. Bounded by
// ctx; a deadline/cancel just ends the drain early (best-effort). Returns bytes read.
func warmRange(ctx context.Context, provider engine.FileProvider, offset, n, size int64) int64 {
	if offset < 0 {
		offset = 0
	}
	if size > 0 && offset >= size {
		return 0
	}
	if size > 0 && offset+n > size {
		n = size - offset
	}
	if n <= 0 {
		return 0
	}
	rd := provider.NewFileReader(ctx)
	defer rd.Close()
	if offset > 0 {
		if _, err := rd.Seek(offset, io.SeekStart); err != nil {
			return 0
		}
	}
	read, _ := io.CopyN(io.Discard, rd, n) // best-effort: ctx cancels the underlying fetch
	return read
}

// reportTaskStreamError tells the web a /stream attempt for a download TASK failed
// WITHOUT marking the download itself failed. The web sync (services/agent.ts)
// clears streamRequested + records streamError only when a streamError is present
// (a plain status="failed" on a not-yet-completed task does NOT clear the flag), so
// the stream-status route then returns {failed:true} and the player fails fast
// instead of polling a URL that never arrives (incident 2026-07-19: usenet fallback
// left the mobile stuck on "Iniciando"). Best-effort + time-bounded on a fresh ctx
// so a shutting-down daemon ctx doesn't drop the report.
func reportTaskStreamError(client *agent.Client, taskID, reason string) {
	if client == nil || taskID == "" {
		return
	}
	go func() {
		rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := client.ReportStatus(rctx, agent.StatusUpdate{TaskID: taskID, StreamError: reason}); err != nil {
			log.Printf("[%s] stream-error report failed: %v", agent.ShortID(taskID), err)
		}
	}()
}

// sessErrNotStreamable is reported for a Usenet stream session whose release
// can't be streamed on the fly (compressed/encrypted RAR, password, ambiguous or
// missing video) and has therefore fallen back to a normal Usenet download. It is
// distinct from start_failed so the web can show "downloading, retry shortly"
// instead of a hard error; an older web that doesn't recognise the code treats it
// as a generic failure and the player falls back exactly the same way.
const sessErrNotStreamable = "not_streamable"

// usenetStreamDeps bundles the OnStreamSession closures + long-lived services the
// Usenet stream branch needs. Passed as a struct so the (already huge,
// grandfathered) OnStreamSession closure grows by a single call while all the
// real wiring lives here in a small file that respects the 500-line / gocognit
// gates.
type usenetStreamDeps struct {
	ctx         context.Context
	cfg         config.Config
	agentClient *agent.Client
	usenetDl    *engine.UsenetDownloader
	streamSrv   *engine.StreamServer
	manager     *engine.Manager
	hlsCache    *engine.HLSCache
	startHLS    func(engine.HLSSessionConfig, context.Context, context.CancelFunc)
	failSession func(sessionID, code, message string)
	markReady   func(sessionID string)
}

// handleUsenetStreamSession serves a Usenet stream session on the fly (arranca en
// segundos) when the release is streamable, else falls back CLEANLY to a normal
// Usenet download so playback is never blocked. It is the resilient bridge from
// the daemon's OnStreamSession decision tree to engine.HandleStreamSession.
//
// Setup does NNTP header reads, so it runs off the sync loop in a goroutine; the
// session is registered up front so a duplicate sync within the setup window is a
// no-op (matching the debrid/HLS branches).
func handleUsenetStreamSession(sess agent.StreamSession, d usenetStreamDeps) {
	req := engine.UsenetStreamRequest{
		SessionID:   sess.SessionID,
		NzbID:       sess.NzbID,
		NzbPassword: sess.NzbPassword,
		InfoHash:    sess.InfoHash,
		Title:       sess.FileName,
		PlayMethod:  sess.PlayMethod,
	}
	hooks := engine.UsenetStreamHooks{
		Direct:   func(h *engine.UsenetStreamHandle) { d.serveUsenetDirect(sess, h) },
		HLS:      func(h *engine.UsenetStreamHandle) { d.serveUsenetHLS(sess, h) },
		Fallback: func(reason string) { d.fallbackUsenetToDownload(sess, reason) },
	}

	// Placeholder cancel so a duplicate sync during setup is deduped by has().
	// Overwritten by the real (handle-closing) cancel once a transport is wired.
	playerSessionRegistry.add(sess.SessionID, func() {})
	go func() {
		mode := d.usenetDl.HandleStreamSession(d.ctx, req, d.streamSrv, d.cfg.Download.UsenetStreaming, hooks)
		if mode == engine.UsenetStreamNone {
			// Fallback already logged, reported, and (when actionable) queued the
			// batch download. Drop the placeholder so a later real download task
			// for the same content isn't shadowed by a dead stream-session entry.
			playerSessionRegistry.remove(sess.SessionID)
		}
	}()
}

// serveUsenetDirect wires the streamable release's provider onto /stream for
// direct play (HTTP Range, no ffmpeg) and reports the session ready. The
// registered cancel unregisters the /usenet source (handle.Close) and clears the
// served file on session end / daemon drain.
func (d usenetStreamDeps) serveUsenetDirect(sess agent.StreamSession, h *engine.UsenetStreamHandle) {
	d.streamSrv.SetFile(h.Provider, sess.TaskID)
	playerSessionRegistry.add(sess.SessionID, func() {
		h.Close()
		d.streamSrv.ClearFile()
	})
	log.Printf("[usenet-stream %s] direct-play: %s", agent.ShortID(sess.SessionID), h.VideoName)
	d.markReady(sess.SessionID)
}

// serveUsenetHLS hands the release's loopback URL to ffmpeg as an HLS/remux
// SourceURL (Muro 2: a container whose index sits at the tail, served by the NNTP
// reader's cheap random-access Seek). The cache is keyed by info_hash so a
// re-play hits the segment cache even though the loopback token rotates.
func (d usenetStreamDeps) serveUsenetHLS(sess agent.StreamSession, h *engine.UsenetStreamHandle) {
	tcRuntime := buildTranscodeRuntime(d.ctx, d.cfg)
	if tcRuntime.FFmpegPath == "" || tcRuntime.FFprobePath == "" {
		// No ffmpeg → can't remux a tail-index container. Unregister the source and
		// fall back to a normal download rather than leave a dead /usenet source.
		h.Close()
		playerSessionRegistry.remove(sess.SessionID)
		d.fallbackUsenetToDownload(sess, "ffmpeg/ffprobe unavailable for usenet HLS")
		return
	}
	// Wrap the HLS cancel so the /usenet source is unregistered when the session
	// ends (startHLS stores THIS cancel in the registry, replacing the placeholder).
	hlsCtx, baseCancel := context.WithCancel(d.ctx)
	hlsCancel := func() { baseCancel(); h.Close() }
	d.startHLS(engine.HLSSessionConfig{
		SessionID:         sess.SessionID,
		SourceURL:         h.LoopbackURL,
		CacheID:           sess.InfoHash,
		VideoCopy:         sess.VideoCopy,
		FileName:          h.VideoName,
		Quality:           sess.Quality,
		AudioIndex:        sess.AudioIndex,
		BurnSubtitleIndex: sess.BurnSubtitleIndex,
		StartSec:          sess.StartSec,
		Prewarm:           sess.Prewarm,
		Transcode:         tcRuntime,
		Cache:             d.hlsCache,
	}, hlsCtx, hlsCancel)
	log.Printf("[usenet-stream %s] HLS from loopback: %s", agent.ShortID(sess.SessionID), h.VideoName)
}

// fallbackUsenetToDownload is the resilient floor: it reports the stream session
// as not-streamable (so the player stops waiting on a live stream) AND, when
// there is something to act on, queues the intact batch Usenet download so the
// file still arrives and a later playback serves it from disk. Playback is
// degraded with a clear message, never silently broken.
func (d usenetStreamDeps) fallbackUsenetToDownload(sess agent.StreamSession, reason string) {
	sid := agent.ShortID(sess.SessionID)
	d.failSession(sess.SessionID, sessErrNotStreamable,
		fmt.Sprintf("usenet not streamable (%s) — downloading, retry playback shortly", reason))

	// Nothing to resolve a download from → report only (the message already told
	// the user). Avoids submitting a task the downloader can't act on.
	if sess.NzbID == "" && sess.InfoHash == "" {
		log.Printf("[usenet-stream %s] no nzbId/infoHash to download — reported only", sid)
		return
	}

	taskID := sess.TaskID
	if taskID == "" {
		taskID = sess.SessionID
	}
	// If this session is bound to a download TASK the web may be polling for a
	// stream URL (/downloads/stream/status?taskId=), report a stream error on it
	// too so streamRequested is cleared and that poller fails fast — failSession
	// above only reconciles the session-id poller, not the task-id one.
	if sess.TaskID != "" {
		reportTaskStreamError(d.agentClient, sess.TaskID,
			fmt.Sprintf("usenet not streamable (%s) — downloading instead", reason))
	}
	// Mode "download" → the manager routes to the Usenet downloader; a duplicate id
	// (web already queued this) is a no-op via the manager's dedup, so a fallback
	// never double-downloads.
	d.manager.Submit(d.ctx, agent.Task{
		ID:              taskID,
		InfoHash:        sess.InfoHash,
		Title:           sess.FileName,
		NzbID:           sess.NzbID,
		NzbPassword:     sess.NzbPassword,
		PreferredMethod: "usenet",
		Mode:            "download",
	})
	log.Printf("[usenet-stream %s] queued batch usenet download %s", sid, agent.ShortID(taskID))
}

// isUsenetStreamTask reports whether a claimed stream task (Mode="stream") must be
// served by the on-the-fly Usenet streamer rather than the torrent path. The web
// sets preferredMethod="usenet" and a pre-resolved NzbID for a Usenet stream;
// routing it to the torrent branch calls AddMagnet(at.InfoHash) on an empty hash
// and hard-errors. A debrid direct URL (its own branch in handleStreamTask) is
// excluded.
func isUsenetStreamTask(at agent.Task) bool {
	return at.PreferredMethod == "usenet" && at.NzbID != "" && at.DirectURL == ""
}

// handleUsenetStreamTask serves a download-task (Mode="stream",
// PreferredMethod="usenet", NzbID set, no debrid DirectURL) as an on-the-fly
// Usenet stream. It is the download-task counterpart of handleUsenetStreamSession
// (the in-browser player-session path): BOTH reuse the SAME resilient bridge
// (usenetDl.HandleStreamSession) — only the transport + reporting hooks differ.
// Here readiness/URL/errors are reported on the TASK (the web polls
// /downloads/stream/status?taskId=), whereas the session path reports by session
// id. A non-streamable release, or the feature being off, falls back CLEANLY to a
// batch download via the manager (mode="download") so the file still arrives for a
// later play from disk — it NEVER routes to the torrent AddMagnet path.
//
// PlayMethod is forced to "direct": a download-task stream is consumed by external
// players (VLC) over the SAME /stream endpoint the debrid + P2P paths use, and the
// Usenet provider serves any container over HTTP Range via the NNTP reader's cheap
// random-access Seek. The in-browser HLS/remux session path (which needs a session
// id + MarkSessionReady) is neither available nor needed here, so no HLS hook is
// wired.
//
// streamCtx bounds serving (a web stop-stream cancels it); daemonCtx (which
// survives a stop-stream) bounds the fallback batch download so a dropped player
// never aborts the download that replaced the stream.
func handleUsenetStreamTask(streamCtx, daemonCtx context.Context, at agent.Task, task *engine.Task, reporter *engine.ProgressReporter, cfg config.Config, agentClient *agent.Client, usenetDl *engine.UsenetDownloader, manager *engine.Manager, srv *engine.StreamServer) {
	task.ResolvedMethod = engine.MethodUsenet
	task.Transition(engine.StatusResolving)
	reporter.Track(task) // resolving + the served /stream URL reach the web by taskId

	req := engine.UsenetStreamRequest{
		SessionID:   at.ID, // the task id doubles as the /usenet source key
		NzbID:       at.NzbID,
		NzbPassword: at.NzbPassword,
		InfoHash:    at.InfoHash,
		Title:       at.Title,
		PlayMethod:  "direct",
	}

	served := false
	hooks := engine.UsenetStreamHooks{
		Direct: func(h *engine.UsenetStreamHandle) {
			served = true
			srv.SetFile(h.Provider, at.ID)
			task.FileName = h.VideoName
			task.TotalBytes = h.VideoSize

			// Cold-buffer fix: VLC opens by reading the MKV header (front) AND the
			// Cues/SeekHead index (last ~128 KB, at the TAIL of the file) before it
			// paints a frame. On a cold usenet stream those articles aren't fetched
			// yet, so the first open stalls and VLC gives up — the second open works
			// only because the Usenet server has since cached them. Pre-fetch head +
			// tail through the SAME provider reader path /stream uses (warming the
			// upstream server cache + the NNTP connection pool) BEFORE reporting the
			// URL ready, so the FIRST open finds the bytes hot. Best-effort +
			// time-bounded on streamCtx: a slow NNTP never turns this into a hang and
			// a per-task cancel (fix #1) aborts it — we report ready regardless.
			hn, tn, wd := prewarmUsenetHeadTail(streamCtx, h.Provider, usenetPrewarmHeadBytes, usenetPrewarmTailBytes, usenetPrewarmTimeout)
			log.Printf("[%s] usenet stream warm-up: head %s + tail %s in %s",
				agent.ShortID(at.ID), ui.FormatBytes(hn), ui.FormatBytes(tn), wd.Round(time.Millisecond))

			task.SetStreamURL(srv.URLsJSON())
			log.Printf("[%s] stream (usenet direct): %s (%s) url: %s",
				agent.ShortID(at.ID), h.VideoName, ui.FormatBytes(h.VideoSize), srv.URL())
			// A live NNTP stream has no local download to track — mark it servable
			// now (UI shows "ready"), same as the debrid direct-play branch. Release
			// the /usenet source when serving ends.
			task.Transition(engine.StatusCompleted)
			go func() {
				<-streamCtx.Done()
				h.Close()
			}()
		},
		Fallback: func(reason string) {
			// Report a STREAM error (not a task status): the web then clears
			// streamRequested + surfaces the reason, so its stream-status route
			// returns {failed:true} and the player fails fast. A plain
			// status="failed" would NOT clear streamRequested for a still-resolving
			// task, leaving the player hung on "Iniciando" (incident 2026-07-19).
			reportTaskStreamError(agentClient, at.ID,
				"usenet not streamable ("+reason+") — downloading instead")
			// Untrack our resolving snapshot BEFORE the manager re-Tracks the same id
			// as a batch download, so the shared reporter has exactly one entry for
			// the id (no double-tracking). The download still arrives for a later
			// play from disk.
			reporter.Untrack(at.ID)
			manager.Submit(daemonCtx, agent.Task{
				ID:              at.ID,
				InfoHash:        at.InfoHash,
				Title:           at.Title,
				NzbID:           at.NzbID,
				NzbPassword:     at.NzbPassword,
				PreferredMethod: "usenet",
				Mode:            "download",
			})
			log.Printf("[%s] usenet stream not streamable (%s) — reported stream error, queued batch download", agent.ShortID(at.ID), reason)
		},
	}

	// Forced PlayMethod="direct" → the bridge fires exactly Direct (streamable) or
	// Fallback (feature off / not streamable / setup fault); the HLS hook is
	// deliberately unwired and never reached.
	mode := usenetDl.HandleStreamSession(streamCtx, req, srv, cfg.Download.UsenetStreaming, hooks)
	if mode == engine.UsenetStreamDirect && served {
		// Serve until the web stops the stream (or the daemon drains), then send the
		// final (completed) status and untrack. The caller's deferred registry
		// cleanup clears the served file.
		<-streamCtx.Done()
		log.Printf("[%s] stream (usenet) stopped", agent.ShortID(at.ID))
		reporter.ReportFinal(context.Background(), task)
		return
	}
	// Fallback already reported the failure and handed the id to the manager; the
	// stream task is untracked. Nothing else to do here.
}

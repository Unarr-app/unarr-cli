package cmd

import (
	"context"
	"fmt"
	"log"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/engine"
)

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

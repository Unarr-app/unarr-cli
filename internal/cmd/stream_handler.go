package cmd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/engine"
	"github.com/Unarr-app/unarr-cli/internal/ui"
	"github.com/Unarr-app/unarr-cli/internal/vpn"
)

const streamIdleTimeout = 30 * time.Minute

// streamDownloadIdlePause is how long a P2P stream can have zero active player
// connections before the agent pauses its greedy background download. Short
// enough to stop caching a title the user abandoned; long enough to survive a
// seek (which briefly reopens the /stream connection) and to act as a small
// cache-ahead grace after playback stops. The download resumes instantly when a
// player reconnects, and downloaded pieces stay on disk regardless.
const streamDownloadIdlePause = 30 * time.Second

// startIdleGuard monitors the persistent stream server and clears the file after inactivity.
func startIdleGuard(ctx context.Context, srv *engine.StreamServer) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if srv.HasFile() && srv.IdleSince() > streamIdleTimeout {
				taskID := srv.CurrentTaskID()
				short := taskID
				if len(short) > 8 {
					short = short[:8]
				}
				log.Printf("[%s] stream idle timeout (%v no HTTP requests), clearing file", short, streamIdleTimeout)
				// Per-task cancel: only reap the IDLE stream (the one served on the
				// persistent server), never other tasks' live streams or in-flight
				// streamability probes.
				cancelStreamTask(taskID)
				srv.ClearFile()
			}
		}
	}
}

// streamRegistry tracks active stream goroutine contexts for cancellation.
// There is only ONE persistent StreamServer — no per-task servers.
var streamRegistry = struct {
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}{
	cancels: make(map[string]context.CancelFunc),
}

// cancelAllStreamContexts cancels EVERY active stream goroutine (download engines,
// watch reporters, streamability probes, …). This is a nuke — use it ONLY at
// daemon shutdown, where tearing everything down is the intent. For displacing or
// stopping a single stream use cancelStreamTask: a global cancel here would abort
// UNRELATED in-flight work (e.g. another task's ~44s NNTP streamability probe),
// which then fails with "context canceled" and falls back to a full download while
// the web polls the stream forever (incident 2026-07-19: idle-timeout of one
// stream killed another task's probe → mobile stuck on "Iniciando").
// Does NOT touch the persistent server — call srv.Shutdown()/ClearFile() separately.
func cancelAllStreamContexts() {
	streamRegistry.mu.Lock()
	cancels := make(map[string]context.CancelFunc, len(streamRegistry.cancels))
	for k, v := range streamRegistry.cancels {
		cancels[k] = v
		delete(streamRegistry.cancels, k)
	}
	streamRegistry.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
}

// isStreamingTask returns true if there is an active stream goroutine for the given task.
func isStreamingTask(taskID string) bool {
	streamRegistry.mu.Lock()
	defer streamRegistry.mu.Unlock()
	_, ok := streamRegistry.cancels[taskID]
	return ok
}

// cancelStreamTask cancels the stream goroutine for ONE task and its paired watch
// reporter ("watch:"+taskID), leaving every other task's stream/probe untouched.
// A blank taskID (e.g. srv.CurrentTaskID() when nothing is served) is a safe no-op.
// This is the per-task counterpart of cancelAllStreamContexts and the ONLY cancel
// used for displacement (new claim), stop-stream, and the idle guard.
func cancelStreamTask(taskID string) {
	if taskID == "" {
		return
	}
	streamRegistry.mu.Lock()
	toCancel := make([]context.CancelFunc, 0, 2)
	for _, key := range []string{taskID, "watch:" + taskID} {
		if cancel, ok := streamRegistry.cancels[key]; ok {
			toCancel = append(toCancel, cancel)
			delete(streamRegistry.cancels, key)
		}
	}
	streamRegistry.mu.Unlock()

	for _, cancel := range toCancel {
		cancel()
	}
}

// handleStreamTask manages a streaming task lifecycle for active torrent downloads.
// It creates a StreamEngine, buffers, sets the file on the persistent server,
// and reports progress until the task is cancelled or the download completes.
func handleStreamTask(parentCtx context.Context, at agent.Task, reporter *engine.ProgressReporter, cfg config.Config, agentClient *agent.Client, srv *engine.StreamServer, vpnTunnel *vpn.Tunnel, usenetDl *engine.UsenetDownloader, manager *engine.Manager, onStateChange func()) {
	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	// NOTE: we deliberately do NOT cancel prior stream goroutines here. The
	// persistent StreamServer is last-writer-wins (SetFile replaces the file;
	// the deferred ClearFile is guarded by CurrentTaskID), so a displaced prior
	// goroutine simply parks on its own ctx until the 30m idle guard reaps it —
	// cheap. Cancelling them at entry would abort an in-flight debrid HEAD of a
	// concurrently-starting task (size resolution), failing that stream.

	// Register for web-initiated cancellation
	streamRegistry.mu.Lock()
	streamRegistry.cancels[at.ID] = cancel
	streamRegistry.mu.Unlock()
	defer func() {
		streamRegistry.mu.Lock()
		delete(streamRegistry.cancels, at.ID)
		streamRegistry.mu.Unlock()
		// Clear file from persistent server if we're still the current task
		if srv.CurrentTaskID() == at.ID {
			srv.ClearFile()
		}
	}()

	task := engine.NewTaskFromAgent(at)
	// Event-driven uplink: stream tasks transition outside the Manager (which
	// wires this for downloads), so set it here too — resolving/downloading/
	// completed/failed get pushed to the server immediately.
	task.SetOnChange(onStateChange)

	// Usenet on-the-fly stream: the web resolved a Usenet release (preferredMethod
	// "usenet" + a pre-resolved NzbID) and asked to STREAM it. Route it to the SAME
	// resilient Usenet streamer the in-browser player-session path uses — NEVER the
	// torrent path below, whose AddMagnet(at.InfoHash) parses an empty hash and
	// hard-errors ("unhandled xt parameter encoding", the hang this closes). The
	// helper owns its OWN reporter lifecycle (a fallback hands the id to the manager
	// as a batch download; sharing reporter.Track/ReportFinal on one id would
	// collide), so it returns before the torrent-oriented tracking below.
	if isUsenetStreamTask(at) {
		handleUsenetStreamTask(ctx, parentCtx, at, task, reporter, cfg, agentClient, usenetDl, manager, srv)
		return
	}

	task.ResolvedMethod = engine.MethodTorrent
	reporter.Track(task)
	defer reporter.ReportFinal(context.Background(), task)

	// Debrid passthrough: when the web resolved a direct HTTPS link (the torrent
	// is cached on the user's debrid + preferredMethod=debrid), stream FROM that
	// link instead of joining the P2P swarm — served over the SAME /stream
	// endpoint, so VLC / external players consume it identically (and far
	// faster). No HLS transcode here: external players handle any container.
	// Falls through to the P2P StreamEngine below when there is no direct URL.
	if at.DirectURL != "" {
		task.ResolvedMethod = engine.MethodDebrid
		task.Transition(engine.StatusResolving)
		bctx, bcancel := context.WithTimeout(ctx, 15*time.Second)
		// fallbackSize 0 → provider derives size from a HEAD; refresh nil → no
		// task-level link-refresh endpoint exists (the web re-resolves stale
		// debrid URLs at the next claim). A mid-stream expiry just ends the
		// stream and the user re-opens it.
		provider, perr := engine.NewDebridFileProvider(bctx, at.DirectURL, at.DirectFileName, 0, nil)
		bcancel()
		if perr != nil {
			task.ErrorMessage = "debrid stream provider: " + perr.Error()
			task.Transition(engine.StatusFailed)
			return
		}
		srv.SetFile(provider, at.ID)
		task.FileName = provider.FileName()
		task.TotalBytes = provider.FileSize()
		task.SetStreamURL(srv.URLsJSON()) // mutex-safe: the reporter reads it via GetStreamURL
		log.Printf("[%s] stream (debrid): %s (%s) url: %s", at.ID[:8], provider.FileName(), ui.FormatBytes(provider.FileSize()), srv.URL())

		if agentClient != nil {
			watchReporter := engine.NewWatchReporter(agentClient, srv, at.ID)
			go watchReporter.Run(ctx)
		}

		// Debrid serves a complete remote file — there is no download to track,
		// so mark it complete immediately (the UI shows "ready"). The persistent
		// server keeps serving until the idle guard reaps it (30m), same as P2P.
		task.Transition(engine.StatusCompleted)
		<-ctx.Done()
		log.Printf("[%s] stream (debrid) stopped", at.ID[:8])
		return
	}

	// 1. Create StreamEngine. The VPN kill-switch is threaded in: with
	// [downloads.vpn] required=true and no healthy tunnel, NewStreamEngine returns
	// ErrVPNRequired so a torrent-only (non-debrid) title is NEVER streamed in the
	// clear — the daemon is the trust boundary and self-protects regardless of what
	// the web sent. Debrid direct-play (at.DirectURL) returned above and is unaffected.
	eng, err := engine.NewStreamEngine(engine.StreamConfig{
		DataDir:     cfg.Download.Dir,
		MetaTimeout: 60 * time.Second,
		VPNTunnel:   vpnTunnel,
		VPNRequired: cfg.Download.VPN.Required,
	})
	if err != nil {
		if errors.Is(err, engine.ErrVPNRequired) {
			task.ErrorMessage = "VPN required: tunnel down — P2P streaming disabled (debrid still works)"
			log.Printf("[%s] stream refused: VPN required but tunnel down — not joining the swarm in the clear", at.ID[:8])
		} else {
			task.ErrorMessage = "create stream engine: " + err.Error()
		}
		task.Transition(engine.StatusFailed)
		return
	}
	defer eng.Shutdown(context.Background())

	// 2. Wait for metadata + select file
	task.Transition(engine.StatusResolving)
	if err := eng.Start(ctx, at.InfoHash); err != nil {
		task.ErrorMessage = err.Error()
		task.Transition(engine.StatusFailed)
		return
	}

	task.FileName = eng.FileName()
	task.TotalBytes = eng.FileLength()
	task.Transition(engine.StatusDownloading)

	log.Printf("[%s] stream: %s (%s)", at.ID[:8], eng.FileName(), ui.FormatBytes(eng.FileLength()))

	// 3. Buffer initial data
	if err := eng.WaitBuffer(ctx, nil); err != nil {
		task.ErrorMessage = "buffering failed: " + err.Error()
		task.Transition(engine.StatusFailed)
		return
	}

	// 4. Set file on the persistent stream server (instant, no port binding)
	srv.SetFile(eng, at.ID)
	task.StreamURL = srv.URLsJSON()
	log.Printf("[%s] stream ready: %s (url: %s)", at.ID[:8], eng.FileName(), srv.URL())

	// Pre-descargar los últimos 5 MB del archivo para que el moov atom (MP4)
	// o el seekhead (MKV) estén disponibles cuando VLC los pida al abrir el
	// stream. Sin esto, VLC busca el final del archivo, el lector bloquea
	// esperando piezas no descargadas, y el resultado es pantalla negra en
	// redes remotas donde la latencia amplifica el efecto.
	eng.PrioritizeTail(ctx, 5*1024*1024)

	// 5. Start watch progress reporter
	if agentClient != nil {
		watchReporter := engine.NewWatchReporter(agentClient, srv, at.ID)
		go watchReporter.Run(ctx)
	}

	// 6. Progress loop until download completes or cancelled
	eng.StartProgressLoop(ctx)
	progressTicker := time.NewTicker(3 * time.Second)
	defer progressTicker.Stop()
	completed := false

	for {
		select {
		case <-ctx.Done():
			log.Printf("[%s] stream stopped", at.ID[:8])
			return

		case <-progressTicker.C:
			// Kill-switch: a mid-stream tunnel drop while still fetching must stop the
			// P2P stream so no pieces are pulled over the real IP. The engine's dials
			// already fail closed (tunnel-routed), but tear the session down
			// explicitly rather than serve a stalled stream. Once completed there is
			// nothing left to fetch, so a later drop can't leak.
			if !completed && !eng.VPNStillHealthy() {
				task.ErrorMessage = "VPN tunnel down — P2P streaming stopped (no clear-net leak)"
				task.Transition(engine.StatusFailed)
				log.Printf("[%s] VPN tunnel went down mid-stream — stopping P2P (partial kept, P2P disabled)", at.ID[:8])
				return
			}

			p := eng.Progress()
			task.UpdateProgress(engine.Progress{
				DownloadedBytes: p.DownloadedBytes,
				TotalBytes:      p.TotalBytes,
				SpeedBps:        p.SpeedBps,
				Peers:           p.Peers,
				Seeds:           p.Seeds,
				FileName:        p.FileName,
			})

			pct := 0
			if p.TotalBytes > 0 {
				pct = int(float64(p.DownloadedBytes) / float64(p.TotalBytes) * 100)
			}

			// Terminal progress
			if !completed && p.TotalBytes > 0 {
				fmt.Fprintf(os.Stderr, "\r[%s] %d%% — %s/%s @ %s/s  peers:%d seeds:%d",
					at.ID[:8], pct,
					ui.FormatBytes(p.DownloadedBytes), ui.FormatBytes(p.TotalBytes), ui.FormatBytes(p.SpeedBps),
					p.Peers, p.Seeds)
			}

			// Idle guard: pause the greedy whole-file download so the agent does
			// not cache a whole title nobody is watching. Two triggers:
			//   1. Displaced — a newer stream took over the persistent server
			//      (CurrentTaskID moved on); nobody can reach our file anymore, so
			//      pause at once. (Without this, switching titles would leave the
			//      old title downloading to 100% in the background — the single
			//      most common abandonment.)
			//   2. Read-idle — we are still the served file but no bytes have been
			//      served for the grace window: player stopped, closed, or paused.
			//      ReadIdleSince() is byte-based, so a paused player whose TCP
			//      socket lingers still counts as idle (a plain connection count
			//      would not).
			// A connected reader keeps fetching its own readahead regardless of
			// the file's base priority, so pausing never interrupts live playback;
			// resume is instant on the next byte served. Skip once completed.
			if !completed {
				displaced := srv.CurrentTaskID() != at.ID
				idle := displaced || srv.ReadIdleSince() >= streamDownloadIdlePause
				switch {
				case idle && !eng.IsDownloadPaused():
					reason := "no active viewer"
					if displaced {
						reason = "stream replaced by a newer title"
					}
					eng.PauseDownload()
					log.Printf("[%s] %s — pausing background download at %d%% (partial kept, resumes on replay)",
						at.ID[:8], reason, pct)
				case !idle && eng.IsDownloadPaused():
					eng.ResumeDownload()
					log.Printf("[%s] viewer active — resuming background download", at.ID[:8])
				}
			}

			if !completed && p.DownloadedBytes >= p.TotalBytes && p.TotalBytes > 0 {
				fmt.Fprint(os.Stderr, "\r\033[2K")
				task.Transition(engine.StatusCompleted)
				log.Printf("[%s] stream download complete, server stays up until idle (30m)", at.ID[:8])
				completed = true
			}
		}
	}
}

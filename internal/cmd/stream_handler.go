package cmd

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/torrentclaw/unarr/internal/agent"
	"github.com/torrentclaw/unarr/internal/config"
	"github.com/torrentclaw/unarr/internal/engine"
	"github.com/torrentclaw/unarr/internal/ui"
)

const streamIdleTimeout = 30 * time.Minute

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
				cancelStreamContexts()
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

// cancelStreamContexts cancels all active stream goroutines (download engines, etc.).
// Does NOT touch the persistent server — call srv.ClearFile() separately if needed.
func cancelStreamContexts() {
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

// cancelStreamTask cancels a specific stream goroutine.
func cancelStreamTask(taskID string) {
	streamRegistry.mu.Lock()
	cancel, ok := streamRegistry.cancels[taskID]
	delete(streamRegistry.cancels, taskID)
	streamRegistry.mu.Unlock()

	if ok {
		cancel()
	}
}

// handleStreamTask manages a streaming task lifecycle for active torrent downloads.
// It creates a StreamEngine, buffers, sets the file on the persistent server,
// and reports progress until the task is cancelled or the download completes.
func handleStreamTask(parentCtx context.Context, at agent.Task, reporter *engine.ProgressReporter, cfg config.Config, agentClient *agent.Client, srv *engine.StreamServer, onStateChange func()) {
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

	// 1. Create StreamEngine
	eng, err := engine.NewStreamEngine(engine.StreamConfig{
		DataDir:     cfg.Download.Dir,
		MetaTimeout: 60 * time.Second,
	})
	if err != nil {
		task.ErrorMessage = "create stream engine: " + err.Error()
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
			p := eng.Progress()
			task.UpdateProgress(engine.Progress{
				DownloadedBytes: p.DownloadedBytes,
				TotalBytes:      p.TotalBytes,
				SpeedBps:        p.SpeedBps,
				Peers:           p.Peers,
				Seeds:           p.Seeds,
				FileName:        p.FileName,
			})

			// Terminal progress
			if !completed && p.TotalBytes > 0 {
				pct := int(float64(p.DownloadedBytes) / float64(p.TotalBytes) * 100)
				fmt.Fprintf(os.Stderr, "\r[%s] %d%% — %s/%s @ %s/s  peers:%d seeds:%d",
					at.ID[:8], pct,
					ui.FormatBytes(p.DownloadedBytes), ui.FormatBytes(p.TotalBytes), ui.FormatBytes(p.SpeedBps),
					p.Peers, p.Seeds)
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

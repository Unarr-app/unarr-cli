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
func handleStreamTask(parentCtx context.Context, at agent.Task, reporter *engine.ProgressReporter, cfg config.Config, agentClient *agent.Client, srv *engine.StreamServer) {
	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

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
	task.ResolvedMethod = engine.MethodTorrent
	reporter.Track(task)
	defer reporter.ReportFinal(context.Background(), task)

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

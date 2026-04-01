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

// startIdleGuard monitors a stream server and cancels the task after 30 minutes of inactivity.
func startIdleGuard(ctx context.Context, srv *engine.StreamServer, taskID string) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if srv.IdleSince() > 30*time.Minute {
				log.Printf("[%s] stream idle timeout (30m no HTTP requests), shutting down", taskID[:8])
				cancelStreamTask(taskID)
				return
			}
		}
	}
}

// streamRegistry tracks active stream tasks and servers for cancellation.
var streamRegistry = struct {
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	servers map[string]*engine.StreamServer // servers for active download streams
}{
	cancels: make(map[string]context.CancelFunc),
	servers: make(map[string]*engine.StreamServer),
}

// cancelAllStreams cancels all active stream tasks and servers (only 1 stream at a time).
func cancelAllStreams() {
	streamRegistry.mu.Lock()
	for taskID, cancel := range streamRegistry.cancels {
		cancel()
		delete(streamRegistry.cancels, taskID)
	}
	for taskID, srv := range streamRegistry.servers {
		srv.Shutdown(context.Background())
		delete(streamRegistry.servers, taskID)
	}
	streamRegistry.mu.Unlock()
}

// cancelStreamTask cancels a running stream task and shuts down any stream server.
func cancelStreamTask(taskID string) {
	streamRegistry.mu.Lock()
	if cancel, ok := streamRegistry.cancels[taskID]; ok {
		cancel()
		delete(streamRegistry.cancels, taskID)
	}
	if srv, ok := streamRegistry.servers[taskID]; ok {
		srv.Shutdown(context.Background())
		delete(streamRegistry.servers, taskID)
	}
	streamRegistry.mu.Unlock()
}

// handleStreamTask manages a streaming task lifecycle outside the Manager.
// It creates a StreamEngine, buffers, starts an HTTP server, and reports
// progress until the task is cancelled or the download completes.
func handleStreamTask(parentCtx context.Context, at agent.Task, reporter *engine.ProgressReporter, cfg config.Config, agentClient *agent.Client) {
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

	// 4. Start HTTP server
	srv := engine.NewStreamServer(eng, cfg.Download.StreamPort)
	streamURL, err := srv.Start(ctx)
	if err != nil {
		task.ErrorMessage = "start HTTP server: " + err.Error()
		task.Transition(engine.StatusFailed)
		return
	}
	defer srv.Shutdown(context.Background())

	// 5. Report stream URL — the reporter will send this to the web
	task.StreamURL = streamURL
	log.Printf("[%s] stream ready: %s", at.ID[:8], streamURL)

	// 5b. Start watch progress reporter (tracks Range requests for playback position)
	if agentClient != nil {
		watchReporter := engine.NewWatchReporter(agentClient, srv, at.ID)
		go watchReporter.Run(ctx)
	}

	// 6. Start idle guard + progress loop
	go startIdleGuard(ctx, srv, at.ID)
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

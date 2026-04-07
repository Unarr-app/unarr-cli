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

// startIdleGuard monitors a stream server and cancels the task after inactivity.
func startIdleGuard(ctx context.Context, srv *engine.StreamServer, taskID string) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if srv.IdleSince() > streamIdleTimeout {
				log.Printf("[%s] stream idle timeout (%v no HTTP requests), shutting down", taskID[:8], streamIdleTimeout)
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
	cancels := make(map[string]context.CancelFunc, len(streamRegistry.cancels))
	for k, v := range streamRegistry.cancels {
		cancels[k] = v
		delete(streamRegistry.cancels, k)
	}
	servers := make(map[string]*engine.StreamServer, len(streamRegistry.servers))
	for k, v := range streamRegistry.servers {
		servers[k] = v
		delete(streamRegistry.servers, k)
	}
	streamRegistry.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
	for _, srv := range servers {
		srv.Shutdown(context.Background())
	}
}

// isStreamingTask returns true if there is an active stream (goroutine or server) for the given task.
func isStreamingTask(taskID string) bool {
	streamRegistry.mu.Lock()
	defer streamRegistry.mu.Unlock()
	_, inCancels := streamRegistry.cancels[taskID]
	_, inServers := streamRegistry.servers[taskID]
	return inCancels || inServers
}

// cancelStreamTask cancels a running stream task and shuts down any stream server.
func cancelStreamTask(taskID string) {
	streamRegistry.mu.Lock()
	cancel, hasCancel := streamRegistry.cancels[taskID]
	delete(streamRegistry.cancels, taskID)
	srv, hasSrv := streamRegistry.servers[taskID]
	delete(streamRegistry.servers, taskID)
	streamRegistry.mu.Unlock()

	if hasCancel {
		cancel()
	}
	if hasSrv {
		srv.Shutdown(context.Background())
	}
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
	streamRegistry.mu.Lock()
	streamRegistry.servers[at.ID] = srv
	streamRegistry.mu.Unlock()
	defer func() {
		srv.Shutdown(context.Background())
		streamRegistry.mu.Lock()
		delete(streamRegistry.servers, at.ID)
		streamRegistry.mu.Unlock()
	}()

	// 5. Report stream URLs — JSON with all network options for smart resolution
	task.StreamURL = srv.URLsJSON()
	log.Printf("[%s] stream ready: %s (primary: %s)", at.ID[:8], task.StreamURL, streamURL)

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

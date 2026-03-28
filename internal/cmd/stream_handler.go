package cmd

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/torrentclaw/torrentclaw-cli/internal/agent"
	"github.com/torrentclaw/torrentclaw-cli/internal/config"
	"github.com/torrentclaw/torrentclaw-cli/internal/engine"
	"github.com/torrentclaw/torrentclaw-cli/internal/ui"
)

// streamRegistry tracks active stream tasks and servers for cancellation.
var streamRegistry = struct {
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	servers map[string]*engine.StreamServer // servers for active download streams
}{
	cancels: make(map[string]context.CancelFunc),
	servers: make(map[string]*engine.StreamServer),
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
func handleStreamTask(parentCtx context.Context, at agent.Task, reporter *engine.ProgressReporter, cfg config.Config) {
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
	srv := engine.NewStreamServer(eng, 0)
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

	// 6. Progress loop
	eng.StartProgressLoop(ctx)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("[%s] stream stopped", at.ID[:8])
			return
		case <-ticker.C:
			p := eng.Progress()
			task.UpdateProgress(engine.Progress{
				DownloadedBytes: p.DownloadedBytes,
				TotalBytes:      p.TotalBytes,
				SpeedBps:        p.SpeedBps,
				Peers:           p.Peers,
				Seeds:           p.Seeds,
				FileName:        p.FileName,
			})
			if p.DownloadedBytes >= p.TotalBytes && p.TotalBytes > 0 {
				task.Transition(engine.StatusCompleted)
				log.Printf("[%s] stream download complete, server stays up until cancelled", at.ID[:8])
				// Don't return — keep HTTP server running so the player
				// can finish reading. The stream stops when the user
				// cancels from the web or the daemon shuts down.
				<-ctx.Done()
				return
			}
		}
	}
}


package engine

import (
	"context"
	"log"
	"time"

	"github.com/torrentclaw/unarr/internal/agent"
)

// WatchReporter periodically sends watch progress to the API based on
// HTTP Range request tracking from the StreamServer.
type WatchReporter struct {
	client      *agent.Client
	server      *StreamServer
	taskID      string
	lastSentPct int // last progress percentage reported (0-100)
}

// NewWatchReporter creates a reporter that tracks playback progress via Range offsets.
func NewWatchReporter(client *agent.Client, server *StreamServer, taskID string) *WatchReporter {
	return &WatchReporter{
		client: client,
		server: server,
		taskID: taskID,
	}
}

// Run reports watch progress every 10 seconds until the context is cancelled.
// A final report is sent on shutdown using a short independent timeout.
func (wr *WatchReporter) Run(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Final report on shutdown — use background context since parent is cancelled.
			finalCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			wr.sendReport(finalCtx)
			cancel()
			return
		case <-ticker.C:
			wr.sendReport(ctx)
		}
	}
}

func (wr *WatchReporter) sendReport(ctx context.Context) {
	pct, durSec := wr.server.EstimatedProgress()
	if pct == 0 || pct == wr.lastSentPct {
		return
	}

	wr.lastSentPct = pct
	update := agent.WatchProgressUpdate{
		TaskID:   wr.taskID,
		Source:   "range",
		Progress: &pct,
	}
	if durSec > 0 {
		update.Duration = &durSec
		pos := int(float64(pct) / 100 * float64(durSec))
		update.Position = &pos
	}

	reportCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := wr.client.ReportWatchProgress(reportCtx, update); err != nil {
		log.Printf("[%s] watch-progress: report failed: %v", wr.taskID[:8], err)
	}
}

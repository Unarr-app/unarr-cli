package cmd

import (
	"context"
	"log"
	"time"

	"github.com/torrentclaw/unarr/internal/agent"
	"github.com/torrentclaw/unarr/internal/engine"
)

// handleSeedFileTask wraps an arbitrary on-disk file as a single-file
// torrent and adds it to the existing torrent client so the WebRTC
// peer can serve pieces to a browser. Reports the generated info_hash
// back to the server so the web player can target /stream/<hash>.
//
// Runs in its own goroutine; never blocks the claim batch.
func handleSeedFileTask(t agent.Task, dl *engine.TorrentDownloader, client *agent.Client) {
	short := agent.ShortID(t.ID)

	if t.FilePath == "" {
		log.Printf("[%s] seed_file: missing filePath, marking failed", short)
		reportSeedFileFailed(client, t.ID, "Missing filePath")
		return
	}

	log.Printf("[%s] seed_file: building torrent from %s", short, t.FilePath)
	hash, err := engine.SeedFileOnDownloader(dl, t.FilePath)
	if err != nil {
		log.Printf("[%s] seed_file: %v", short, err)
		reportSeedFileFailed(client, t.ID, err.Error())
		return
	}

	infoHash := hash.HexString()
	log.Printf("[%s] seed_file: seeding ih=%s", short, infoHash)

	// Push the info_hash + downloading status (file is on disk; from the
	// client's perspective it's already complete). The web side polls
	// /api/internal/stream/seed-file/<taskId> waiting for this update.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, reportErr := client.ReportStatus(ctx, agent.StatusUpdate{
		TaskID:   t.ID,
		Status:   "downloading", // semantic: actively serving
		InfoHash: infoHash,
		FilePath: t.FilePath,
	})
	if reportErr != nil {
		log.Printf("[%s] seed_file: failed to push info_hash: %v", short, reportErr)
	}
}

func reportSeedFileFailed(client *agent.Client, taskID, msg string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := client.ReportStatus(ctx, agent.StatusUpdate{
		TaskID:       taskID,
		Status:       "failed",
		ErrorMessage: msg,
	})
	if err != nil {
		log.Printf("[%s] seed_file: report-failed itself failed: %v", agent.ShortID(taskID), err)
	}
}

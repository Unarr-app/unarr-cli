package download_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/torrentclaw/torrentclaw-cli/internal/usenet/download"
	"github.com/torrentclaw/torrentclaw-cli/internal/usenet/nntp"
	"github.com/torrentclaw/torrentclaw-cli/internal/usenet/nzb"
	"github.com/torrentclaw/torrentclaw-cli/internal/usenet/postprocess"
)

// TestE2EDownload is a real end-to-end test that downloads from Usenet.
// It requires:
//   - NZB file at /tmp/oppenheimer-test.nzb
//   - NNTP credentials in env: NNTP_USER, NNTP_PASS
//   - Network access to reader.torrentclaw.com:563
//
// Run with: go test -v -run TestE2EDownload -tags e2e -timeout 30m ./internal/usenet/download/
func TestE2EDownload(t *testing.T) {
	if os.Getenv("NNTP_USER") == "" || os.Getenv("NNTP_PASS") == "" {
		t.Skip("NNTP_USER and NNTP_PASS not set — skipping e2e test")
	}

	nzbPath := os.Getenv("NZB_FILE")
	if nzbPath == "" {
		nzbPath = "/tmp/oppenheimer-test.nzb"
	}

	// 1. Parse NZB
	f, err := os.Open(nzbPath)
	if err != nil {
		t.Fatalf("open NZB: %v", err)
	}
	defer f.Close()

	nzbFile, err := nzb.Parse(f)
	if err != nil {
		t.Fatalf("parse NZB: %v", err)
	}

	t.Logf("NZB: %d files, %d total segments, %.2f GB",
		len(nzbFile.Files), nzbFile.TotalSegments(),
		float64(nzbFile.TotalBytes())/1024/1024/1024)

	if nzbFile.Password != "" {
		t.Logf("NZB password: %s", nzbFile.Password)
	}

	t.Logf("Has rars: %v, Has par2: %v", nzbFile.HasRars(), nzbFile.HasPar2())

	for _, file := range nzbFile.Files {
		t.Logf("  %s — %d segments, %.1f MB",
			file.Filename(), len(file.Segments),
			float64(file.TotalBytes())/1024/1024)
	}

	// 2. Connect NNTP
	client := nntp.NewClient(nntp.Config{
		Host:           "reader.torrentclaw.com",
		Port:           563,
		SSL:            true,
		TLSServerName:  "xsnews.nl",
		Username:       os.Getenv("NNTP_USER"),
		Password:       os.Getenv("NNTP_PASS"),
		MaxConnections: 10,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	t.Log("Connecting NNTP (10 connections)...")
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("NNTP connect: %v", err)
	}
	defer client.Close()
	t.Logf("NNTP connected: %s", client.Status())

	// 3. Download all files
	outputDir, err := os.MkdirTemp("", "usenet-e2e-*")
	if err != nil {
		t.Fatalf("tmpdir: %v", err)
	}
	t.Logf("Output dir: %s", outputDir)
	// Don't cleanup automatically — let user inspect
	// defer os.RemoveAll(outputDir)

	dl := download.NewDownloader(client)

	progressCh := make(chan download.Progress, 64)
	go func() {
		for p := range progressCh {
			pct := 0
			if p.BytesTotal > 0 {
				pct = int(float64(p.BytesDownloaded) / float64(p.BytesTotal) * 100)
			}
			fmt.Fprintf(os.Stderr, "\r  [%s] %d%% — %s/%s @ %s/s (%d/%d segs)  ",
				p.FileName,
				pct,
				formatSize(p.BytesDownloaded),
				formatSize(p.BytesTotal),
				formatSize(p.SpeedBps),
				p.SegmentsDone, p.SegmentsTotal)
		}
		fmt.Fprintln(os.Stderr)
	}()

	downloadedFiles, err := dl.DownloadNZB(ctx, nzbFile, outputDir, nil, progressCh)
	close(progressCh)
	if err != nil {
		t.Fatalf("download: %v", err)
	}

	t.Logf("Downloaded %d files:", len(downloadedFiles))
	for name, path := range downloadedFiles {
		fi, _ := os.Stat(path)
		size := int64(0)
		if fi != nil {
			size = fi.Size()
		}
		t.Logf("  %s → %s (%.1f MB)", name, path, float64(size)/1024/1024)
	}

	// 4. Post-process
	t.Log("Post-processing...")
	result, err := postprocess.Process(outputDir, downloadedFiles, postprocess.Options{
		Password: nzbFile.Password,
		Cleanup:  true,
	})
	if err != nil {
		t.Fatalf("post-process: %v", err)
	}

	t.Logf("Post-process result:")
	t.Logf("  Final path: %s", result.FinalPath)
	t.Logf("  Repaired: %v", result.Repaired)
	t.Logf("  Extracted: %v", result.Extracted)
	t.Logf("  Files: %v", result.Files)

	// Verify final file exists and has size
	if result.FinalPath != "" {
		fi, err := os.Stat(result.FinalPath)
		if err != nil {
			t.Errorf("final file stat: %v", err)
		} else {
			t.Logf("  Final size: %.2f GB", float64(fi.Size())/1024/1024/1024)
		}
	}

	// List all files in output dir
	t.Log("Final directory contents:")
	filepath.Walk(outputDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(outputDir, path)
		t.Logf("  %s (%.1f MB)", rel, float64(info.Size())/1024/1024)
		return nil
	})
}

func formatSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

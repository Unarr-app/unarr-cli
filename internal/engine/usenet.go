package engine

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/torrentclaw/unarr/internal/agent"
	"github.com/torrentclaw/unarr/internal/config"
	"github.com/torrentclaw/unarr/internal/usenet/download"
	"github.com/torrentclaw/unarr/internal/usenet/nntp"
	"github.com/torrentclaw/unarr/internal/usenet/nzb"
	"github.com/torrentclaw/unarr/internal/usenet/postprocess"
)

// activeDownload holds the state for a single in-progress usenet download.
type activeDownload struct {
	cancel  context.CancelFunc
	taskDir string                    // populated after MkdirAll; empty before
	tracker *download.ProgressTracker // populated after tracker creation; nil before
}

// UsenetDownloader downloads via Usenet/NZB protocol.
// It searches for NZBs, downloads articles via NNTP, and assembles the final files.
type UsenetDownloader struct {
	apiClient *agent.Client
	enabled   bool // set during initialization based on features

	mu         sync.Mutex
	nntpClient *nntp.Client
	active     map[string]*activeDownload

	// Cached credentials
	credentials *agent.UsenetCredentials
	credExpiry  time.Time

	// Cached NZB search results (from Available → Download)
	nzbCache   map[string]*agent.NzbSearchResult // taskID → best result
	nzbCacheMu sync.RWMutex
}

// NewUsenetDownloader creates a usenet downloader.
// apiClient is used to call the web API for NZB search, download, and credentials.
func NewUsenetDownloader(apiClient *agent.Client) *UsenetDownloader {
	return &UsenetDownloader{
		apiClient: apiClient,
		enabled:   true,
		active:    make(map[string]*activeDownload),
		nzbCache:  make(map[string]*agent.NzbSearchResult),
	}
}

func (u *UsenetDownloader) Method() DownloadMethod { return MethodUsenet }

// SetEnabled controls whether usenet downloads are available.
func (u *UsenetDownloader) SetEnabled(enabled bool) {
	u.mu.Lock()
	u.enabled = enabled
	u.mu.Unlock()
}

// Available checks if a usenet download is possible for this task.
// Searches NZB indexers by IMDb ID or title and caches the result.
func (u *UsenetDownloader) Available(ctx context.Context, task *Task) (bool, error) {
	u.mu.Lock()
	enabled := u.enabled
	u.mu.Unlock()

	if !enabled {
		return false, nil
	}

	// Need at least an IMDb ID or title to search
	if task.IMDbID == "" && task.Title == "" {
		return false, nil
	}

	// If task has pre-resolved NZB ID, it's available
	if task.NzbID != "" {
		return true, nil
	}

	// Search NZB indexers
	result, err := u.searchBestNzb(ctx, task)
	if err != nil {
		return false, nil // search failure = not available (don't error out)
	}
	if result == nil {
		return false, nil
	}

	// Cache for Download()
	u.nzbCacheMu.Lock()
	u.nzbCache[task.ID] = result
	u.nzbCacheMu.Unlock()

	return true, nil
}

// Download performs the full usenet download pipeline:
// search NZB → download NZB file → parse → NNTP download → assemble → post-process.
func (u *UsenetDownloader) Download(ctx context.Context, task *Task, outputDir string, progressCh chan<- Progress) (*Result, error) {
	// Create cancellable context
	dlCtx, cancel := context.WithCancel(ctx)

	dl := &activeDownload{cancel: cancel}
	u.mu.Lock()
	u.active[task.ID] = dl
	u.mu.Unlock()

	defer func() {
		u.mu.Lock()
		delete(u.active, task.ID)
		u.mu.Unlock()
		cancel()
	}()

	shortID := task.ID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}

	// Step 1: Get NZB ID (from cache, task, or search)
	nzbID, nzbTitle, err := u.resolveNzbID(dlCtx, task)
	if err != nil {
		return nil, fmt.Errorf("resolve NZB: %w", err)
	}

	log.Printf("[%s] NZB: %s", shortID, nzbTitle)

	// Step 2: Download NZB file (or use cached version for resume)
	resumeDir := filepath.Join(config.DataDir(), "resume")
	nzbCachePath := filepath.Join(resumeDir, task.ID+".nzb")

	nzbData, err := os.ReadFile(nzbCachePath)
	if err != nil {
		// Not cached — download from server
		nzbData, err = u.apiClient.DownloadNzb(dlCtx, nzbID)
		if err != nil {
			return nil, fmt.Errorf("download NZB: %w", err)
		}
		// Cache for future resume (best-effort — download still works without cache)
		if mkErr := os.MkdirAll(resumeDir, 0o755); mkErr != nil {
			log.Printf("[%s] resume dir create failed: %v", shortID, mkErr)
		} else if wErr := os.WriteFile(nzbCachePath, nzbData, 0o644); wErr != nil {
			log.Printf("[%s] NZB cache write failed: %v", shortID, wErr)
		}
	} else {
		log.Printf("[%s] using cached NZB", shortID)
	}

	// Step 3: Parse NZB
	nzbFile, err := nzb.ParseBytes(nzbData)
	if err != nil {
		return nil, fmt.Errorf("parse NZB: %w", err)
	}

	totalBytes := nzbFile.TotalBytes()
	totalSegs := nzbFile.TotalSegments()
	log.Printf("[%s] NZB parsed: %d files, %d segments, %s",
		shortID, len(nzbFile.Files), totalSegs, formatBytes(totalBytes))

	// Step 3.5: Resume support — load or create progress tracker
	tracker := download.NewProgressTracker(task.ID, nzbFile, resumeDir)
	resumed, _ := tracker.Load()
	if resumed {
		log.Printf("[%s] resuming usenet download (%d/%d segments completed)",
			shortID, tracker.TotalCompleted(), totalSegs)
	}

	// Always flush progress on exit — covers graceful shutdown, SIGTERM,
	// error returns, and shutdown-timeout scenarios. The atomic write
	// (tmp+rename) ensures the file is never corrupted even on hard kill.
	defer tracker.Flush()

	// Step 4: Get NNTP credentials and connect
	creds, err := u.getCredentials(dlCtx)
	if err != nil {
		return nil, fmt.Errorf("get credentials: %w", err)
	}

	nntpClient, err := u.getOrCreateNNTP(dlCtx, creds)
	if err != nil {
		return nil, fmt.Errorf("NNTP connect: %w", err)
	}

	log.Printf("[%s] NNTP: %s", shortID, nntpClient.Status())

	// Step 5: Create download directory for this task
	taskDir := filepath.Join(outputDir, sanitizeDir(task.Title))
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		return nil, fmt.Errorf("create dir: %w", err)
	}

	// Register tracker and taskDir for Cancel() cleanup
	u.mu.Lock()
	dl.taskDir = taskDir
	dl.tracker = tracker
	u.mu.Unlock()

	// Step 6: Download all files via NNTP
	segDl := download.NewDownloader(nntpClient)

	// Bridge download.Progress to engine.Progress
	dlProgressCh := make(chan download.Progress, 16)
	go func() {
		for dp := range dlProgressCh {
			p := Progress{
				DownloadedBytes: dp.BytesDownloaded,
				TotalBytes:      dp.BytesTotal,
				SpeedBps:        dp.SpeedBps,
				FileName:        dp.FileName,
			}
			if dp.BytesTotal > 0 {
				p.ETA = int(float64(dp.BytesTotal-dp.BytesDownloaded) / float64(max(dp.SpeedBps, 1)))
			}
			task.UpdateProgress(p)
			select {
			case progressCh <- p:
			default:
			}
		}
	}()

	downloadedFiles, err := segDl.DownloadNZB(dlCtx, nzbFile, taskDir, tracker, dlProgressCh)
	close(dlProgressCh)

	if err != nil {
		return nil, fmt.Errorf("NNTP download: %w", err)
	}

	// Step 7: Post-processing (par2, extract, cleanup)
	log.Printf("[%s] post-processing...", shortID)

	// Use password from NZB meta (embedded in file), or from task (user-provided)
	password := nzbFile.Password
	if task.NzbPassword != "" {
		password = task.NzbPassword // user-provided overrides NZB meta
	}
	if password != "" {
		log.Printf("[%s] NZB has password: %s", shortID, password)
	}
	ppResult, err := postprocess.Process(taskDir, downloadedFiles, postprocess.Options{
		Password: password,
		Cleanup:  true,
	})
	if err != nil {
		// Password error is special — report clearly
		if _, ok := err.(*postprocess.PasswordError); ok {
			return nil, fmt.Errorf("archive is password protected (set password in download options)")
		}
		return nil, fmt.Errorf("post-process: %w", err)
	}

	if ppResult.Repaired {
		log.Printf("[%s] par2: repair was needed and successful", shortID)
	}
	if ppResult.Extracted {
		log.Printf("[%s] extracted archive", shortID)
	}

	finalPath := ppResult.FinalPath
	if finalPath == "" {
		// Fallback: use the task directory
		finalPath = taskDir
	}

	// Get final file size
	var finalSize int64
	if fi, err := os.Stat(finalPath); err == nil {
		finalSize = fi.Size()
	}

	// Clean up resume state on successful completion
	tracker.Remove()

	return &Result{
		FilePath: finalPath,
		FileName: filepath.Base(finalPath),
		Method:   MethodUsenet,
		Size:     finalSize,
	}, nil
}

// Pause cancels an in-progress download but keeps files.
func (u *UsenetDownloader) Pause(taskID string) error {
	u.mu.Lock()
	dl := u.active[taskID]
	u.mu.Unlock()
	if dl != nil {
		dl.cancel()
	}
	return nil
}

// Cancel aborts an in-progress download and removes partial files + resume state.
func (u *UsenetDownloader) Cancel(taskID string) error {
	u.mu.Lock()
	dl := u.active[taskID]
	u.mu.Unlock()

	if dl == nil {
		return nil
	}

	// Cancel context first — workers will stop and release file handles
	dl.cancel()

	// Remove resume state (best-effort)
	if dl.tracker != nil {
		dl.tracker.Remove()
	}

	// Remove partial download directory in background (can be slow for large dirs)
	if dl.taskDir != "" {
		go os.RemoveAll(dl.taskDir)
	}

	return nil
}

// Shutdown closes the NNTP connection pool.
func (u *UsenetDownloader) Shutdown(_ context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	// Cancel all active downloads
	for id, dl := range u.active {
		dl.cancel()
		delete(u.active, id)
	}

	// Close NNTP
	if u.nntpClient != nil {
		u.nntpClient.Close()
		u.nntpClient = nil
	}

	return nil
}

// --- Internal helpers ---

func (u *UsenetDownloader) searchBestNzb(ctx context.Context, task *Task) (*agent.NzbSearchResult, error) {
	params := agent.NzbSearchParams{
		Limit: 10,
	}

	if task.IMDbID != "" {
		params.IMDbID = task.IMDbID
	} else {
		params.Query = task.Title
	}

	resp, err := u.apiClient.SearchNzbs(ctx, params)
	if err != nil {
		return nil, err
	}

	if len(resp.Results) == 0 {
		return nil, nil
	}

	// Pick best match: prefer largest size (likely best quality), then most grabs
	best := &resp.Results[0]
	for i := 1; i < len(resp.Results); i++ {
		r := &resp.Results[i]
		if r.Size > best.Size {
			best = r
		} else if r.Size == best.Size && r.Grabs > best.Grabs {
			best = r
		}
	}

	return best, nil
}

func (u *UsenetDownloader) resolveNzbID(ctx context.Context, task *Task) (string, string, error) {
	// Priority 1: Task has pre-resolved NZB ID
	if task.NzbID != "" {
		return task.NzbID, task.Title, nil
	}

	// Priority 2: Check cache from Available()
	u.nzbCacheMu.RLock()
	cached, ok := u.nzbCache[task.ID]
	u.nzbCacheMu.RUnlock()
	if ok {
		// Clean cache entry
		u.nzbCacheMu.Lock()
		delete(u.nzbCache, task.ID)
		u.nzbCacheMu.Unlock()
		return cached.NzbID, cached.Title, nil
	}

	// Priority 3: Search now
	result, err := u.searchBestNzb(ctx, task)
	if err != nil {
		return "", "", err
	}
	if result == nil {
		return "", "", fmt.Errorf("no NZB found for %q (IMDb: %s)", task.Title, task.IMDbID)
	}
	return result.NzbID, result.Title, nil
}

func (u *UsenetDownloader) getCredentials(ctx context.Context) (*agent.UsenetCredentials, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	// Use cached credentials if still valid
	if u.credentials != nil && time.Now().Before(u.credExpiry) {
		return u.credentials, nil
	}

	creds, err := u.apiClient.GetUsenetCredentials(ctx)
	if err != nil {
		return nil, err
	}

	u.credentials = creds
	u.credExpiry = time.Now().Add(5 * time.Minute)
	return creds, nil
}

func (u *UsenetDownloader) getOrCreateNNTP(ctx context.Context, creds *agent.UsenetCredentials) (*nntp.Client, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.nntpClient != nil {
		return u.nntpClient, nil
	}

	maxConns := creds.MaxConnections
	if maxConns <= 0 {
		maxConns = 10
	}

	client := nntp.NewClient(nntp.Config{
		Host:           creds.Host,
		Port:           creds.Port,
		SSL:            creds.SSL,
		TLSServerName:  creds.TLSServerName,
		Username:       creds.Username,
		Password:       creds.Password,
		MaxConnections: maxConns,
	})

	if err := client.Connect(ctx); err != nil {
		return nil, err
	}

	u.nntpClient = client
	return client, nil
}

func sanitizeDir(name string) string {
	if name == "" {
		return "usenet_download"
	}
	for _, c := range []string{"/", "\\", ":", "*", "?", "\"", "<", ">", "|"} {
		name = strings.ReplaceAll(name, c, "_")
	}
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}

package engine

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	alog "github.com/anacrolix/log"
	"github.com/anacrolix/torrent"
	"golang.org/x/time/rate"
)

var defaultTrackers = []string{
	"udp://tracker.opentrackr.org:1337/announce",
	"udp://open.stealth.si:80/announce",
	"udp://tracker.torrent.eu.org:451/announce",
	"udp://open.demonii.com:1337/announce",
	"udp://exodus.desync.com:6969/announce",
}

// TorrentConfig holds settings for the BitTorrent downloader.
type TorrentConfig struct {
	DataDir          string
	StallTimeout     time.Duration // no progress for this long = stall (default 90s)
	MaxTimeout       time.Duration // absolute maximum per torrent (default 30m)
	MaxDownloadRate  int64         // bytes/s, 0 = unlimited
	MaxUploadRate    int64         // bytes/s, 0 = unlimited
	SeedEnabled      bool
	SeedRatio        float64       // target seed ratio (default 0, meaning seed until SeedTime)
	SeedTime         time.Duration // min seed time after completion (default 0)
}

// TorrentDownloader downloads torrents via BitTorrent P2P.
type TorrentDownloader struct {
	client *torrent.Client
	cfg    TorrentConfig

	activeMu sync.Mutex
	active   map[string]*torrent.Torrent // taskID -> torrent handle
}

// NewTorrentDownloader creates a BitTorrent downloader with a long-lived client.
func NewTorrentDownloader(cfg TorrentConfig) (*TorrentDownloader, error) {
	if cfg.StallTimeout == 0 {
		cfg.StallTimeout = 90 * time.Second
	}
	if cfg.MaxTimeout == 0 {
		cfg.MaxTimeout = 30 * time.Minute
	}

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	tcfg := torrent.NewDefaultClientConfig()
	tcfg.DataDir = cfg.DataDir
	tcfg.Seed = cfg.SeedEnabled
	tcfg.NoUpload = !cfg.SeedEnabled
	tcfg.ListenPort = 0
	tcfg.Logger = alog.Default.FilterLevel(alog.Disabled)

	if cfg.MaxDownloadRate > 0 {
		burst := int(cfg.MaxDownloadRate)
		if burst < 256*1024 {
			burst = 256 * 1024
		}
		tcfg.DownloadRateLimiter = rate.NewLimiter(rate.Limit(cfg.MaxDownloadRate), burst)
	}
	if cfg.MaxUploadRate > 0 {
		burst := int(cfg.MaxUploadRate)
		if burst < 256*1024 {
			burst = 256 * 1024
		}
		tcfg.UploadRateLimiter = rate.NewLimiter(rate.Limit(cfg.MaxUploadRate), burst)
	}

	client, err := torrent.NewClient(tcfg)
	if err != nil {
		return nil, fmt.Errorf("create torrent client: %w", err)
	}

	return &TorrentDownloader{
		client: client,
		cfg:    cfg,
		active: make(map[string]*torrent.Torrent),
	}, nil
}

func (d *TorrentDownloader) Method() DownloadMethod { return MethodTorrent }

func (d *TorrentDownloader) Available(_ context.Context, task *Task) (bool, error) {
	return task.InfoHash != "", nil
}

func (d *TorrentDownloader) Download(ctx context.Context, task *Task, outputDir string, progressCh chan<- Progress) (*Result, error) {
	magnet := buildMagnet(task.InfoHash)

	t, err := d.client.AddMagnet(magnet)
	if err != nil {
		return nil, fmt.Errorf("add magnet: %w", err)
	}

	// Track active torrent
	d.activeMu.Lock()
	d.active[task.ID] = t
	d.activeMu.Unlock()

	cleanup := func() {
		d.activeMu.Lock()
		delete(d.active, task.ID)
		d.activeMu.Unlock()
		if !d.cfg.SeedEnabled {
			t.Drop()
		}
	}

	// 1. Wait for metadata
	log.Printf("[%s] waiting for metadata...", task.ID[:8])
	metaCtx, metaCancel := context.WithTimeout(ctx, d.cfg.StallTimeout)
	defer metaCancel()

	select {
	case <-t.GotInfo():
		log.Printf("[%s] metadata received: %s (%d files)", task.ID[:8], t.Name(), len(t.Files()))
	case <-metaCtx.Done():
		cleanup()
		return nil, fmt.Errorf("metadata timeout after %s", d.cfg.StallTimeout)
	}

	// 2. Select files to download (prefer largest video + matching subs)
	totalBytes, fileName := d.selectFiles(t, task.ID)

	log.Printf("[%s] downloading %s (%s)", task.ID[:8], fileName, formatBytes(totalBytes))

	// 3. Poll progress with stall detection
	result, err := d.pollDownload(ctx, t, task, totalBytes, fileName, progressCh)
	if err != nil {
		cleanup()
		return nil, err
	}

	// 4. Determine file path
	// For multi-file torrents, fileName includes the torrent dir prefix (e.g. "TorrentName/file.mkv").
	// Try the full path first, then just the file inside the torrent dir.
	filePath := filepath.Join(d.cfg.DataDir, fileName)
	if _, statErr := os.Stat(filePath); statErr != nil {
		// File might have been moved — try torrent directory
		dirPath := filepath.Join(d.cfg.DataDir, t.Name())
		if fi, statErr2 := os.Stat(dirPath); statErr2 == nil && fi.IsDir() {
			// Look for the actual file inside the directory
			base := filepath.Base(fileName)
			candidate := filepath.Join(dirPath, base)
			if _, statErr3 := os.Stat(candidate); statErr3 == nil {
				filePath = candidate
			} else {
				filePath = dirPath
			}
		} else {
			filePath = dirPath
		}
	}

	result.FilePath = filePath
	result.FileName = filepath.Base(fileName)
	result.Method = MethodTorrent
	result.Size = totalBytes

	// If seeding enabled, keep alive (don't cleanup).
	// The manager handles seeding lifecycle.
	if !d.cfg.SeedEnabled {
		cleanup()
	}

	return result, nil
}

func (d *TorrentDownloader) pollDownload(ctx context.Context, t *torrent.Torrent, task *Task, totalBytes int64, fileName string, progressCh chan<- Progress) (*Result, error) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	deadline := time.After(d.cfg.MaxTimeout)
	lastBytesAt := time.Now()
	lastBytes := int64(0)

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("cancelled")

		case <-deadline:
			return nil, fmt.Errorf("max timeout %s exceeded", d.cfg.MaxTimeout)

		case <-ticker.C:
			downloaded := t.BytesCompleted()
			now := time.Now()

			// Speed calculation
			speed := downloaded - lastBytes
			if speed < 0 {
				speed = 0
			}

			// Stall detection (dual-level like TrueSpec)
			if downloaded > lastBytes {
				lastBytesAt = now
				lastBytes = downloaded
			} else if now.Sub(lastBytesAt) > d.cfg.StallTimeout {
				stats := t.Stats()
				return nil, fmt.Errorf("stalled: no progress for %s (peers: %d, seeds: %d)",
					d.cfg.StallTimeout, stats.ActivePeers, stats.ConnectedSeeders)
			}

			// ETA
			var eta int
			if speed > 0 {
				remaining := totalBytes - downloaded
				eta = int(remaining / speed)
			}

			// Peer stats
			stats := t.Stats()

			// Terminal progress
			pct := int(float64(downloaded) / float64(totalBytes) * 100)
			fmt.Fprintf(os.Stderr, "\r[%s] %d%% — %s/%s @ %s/s  peers:%d seeds:%d",
				task.ID[:8], pct,
				formatBytes(downloaded), formatBytes(totalBytes), formatBytes(speed),
				stats.ActivePeers, stats.ConnectedSeeders)

			// Report progress
			p := Progress{
				DownloadedBytes: downloaded,
				TotalBytes:      totalBytes,
				SpeedBps:        speed,
				ETA:             eta,
				Peers:           stats.ActivePeers,
				Seeds:           stats.ConnectedSeeders,
				FileName:        fileName,
			}
			task.UpdateProgress(p)

			select {
			case progressCh <- p:
			default: // don't block if channel full
			}

			// Check completion
			if downloaded >= totalBytes {
				fmt.Fprint(os.Stderr, "\r\033[2K") // clear progress line
				log.Printf("[%s] download complete: %s", task.ID[:8], fileName)
				return &Result{}, nil
			}
		}
	}
}

// Pause drops the torrent handle but keeps partial files on disk for resume.
func (d *TorrentDownloader) Pause(taskID string) error {
	d.activeMu.Lock()
	t, ok := d.active[taskID]
	delete(d.active, taskID)
	d.activeMu.Unlock()

	if !ok {
		return nil
	}

	t.Drop()
	log.Printf("[%s] paused (files kept for resume)", taskID[:8])
	return nil
}

// Cancel drops the torrent handle and removes partial files from disk.
func (d *TorrentDownloader) Cancel(taskID string) error {
	d.activeMu.Lock()
	t, ok := d.active[taskID]
	delete(d.active, taskID)
	d.activeMu.Unlock()

	if !ok {
		return nil
	}

	name := t.Name()
	t.Drop()

	if name != "" {
		path, err := safePath(d.cfg.DataDir, name)
		if err != nil {
			log.Printf("[%s] cancel blocked: %v", taskID[:8], err)
			return nil
		}
		if fi, statErr := os.Stat(path); statErr == nil {
			if fi.IsDir() {
				os.RemoveAll(path)
			} else {
				os.Remove(path)
			}
			log.Printf("[%s] cleaned up partial download: %s", taskID[:8], name)
		}
	}

	return nil
}

func (d *TorrentDownloader) Shutdown(ctx context.Context) error {
	d.activeMu.Lock()
	for id, t := range d.active {
		t.Drop()
		delete(d.active, id)
	}
	d.activeMu.Unlock()

	errs := d.client.Close()
	if len(errs) > 0 {
		return fmt.Errorf("close client: %v", errs[0])
	}
	return nil
}

// StartStream starts an HTTP server for an active torrent download.
// It selects the largest video file and serves it via HTTP Range requests.
// Returns the running server (caller is responsible for shutdown).
func (d *TorrentDownloader) StartStream(taskID string) (*StreamServer, error) {
	d.activeMu.Lock()
	t, ok := d.active[taskID]
	d.activeMu.Unlock()

	if !ok {
		return nil, fmt.Errorf("no active torrent for task %s", taskID[:8])
	}

	// Select largest video file
	files := t.Files()
	var video *torrent.File
	for _, f := range files {
		ext := strings.ToLower(filepath.Ext(f.DisplayPath()))
		if VideoExts[ext] && (video == nil || f.Length() > video.Length()) {
			video = f
		}
	}
	if video == nil {
		// No video — use largest file
		for _, f := range files {
			if video == nil || f.Length() > video.Length() {
				video = f
			}
		}
	}
	if video == nil {
		return nil, fmt.Errorf("torrent has no files")
	}

	srv := NewStreamServerFromFile(video, 0)
	url, err := srv.Start(context.Background())
	if err != nil {
		return nil, fmt.Errorf("start stream server: %w", err)
	}

	log.Printf("[%s] stream started: %s → %s", taskID[:8], filepath.Base(video.DisplayPath()), url)
	return srv, nil
}

// VideoExts is the canonical set of video file extensions used for file selection.
var VideoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".m4v": true,
	".wmv": true, ".ts": true, ".webm": true, ".mov": true,
	".mpg": true, ".mpeg": true, ".vob": true, ".flv": true,
}

var subExts = map[string]bool{
	".srt": true, ".ass": true, ".sub": true, ".ssa": true, ".vtt": true,
}

// selectFiles picks the largest video file + matching subtitles.
// Falls back to downloading everything if no video file is found.
// Returns the total bytes to download and the primary file name.
func (d *TorrentDownloader) selectFiles(t *torrent.Torrent, taskID string) (totalBytes int64, fileName string) {
	files := t.Files()

	if len(files) <= 1 {
		t.DownloadAll()
		return t.Length(), t.Name()
	}

	// Find largest video file
	var video *torrent.File
	for _, f := range files {
		ext := strings.ToLower(filepath.Ext(f.DisplayPath()))
		if VideoExts[ext] && (video == nil || f.Length() > video.Length()) {
			video = f
		}
	}

	if video == nil {
		// No video (music, software, etc.) — download everything
		t.DownloadAll()
		return t.Length(), t.Name()
	}

	// Download only the video
	video.Download()
	totalBytes = video.Length()
	fileName = video.DisplayPath()

	// Also download matching subtitles
	videoBase := strings.TrimSuffix(video.DisplayPath(), filepath.Ext(video.DisplayPath()))
	var subCount int
	for _, f := range files {
		ext := strings.ToLower(filepath.Ext(f.DisplayPath()))
		if subExts[ext] {
			fBase := strings.TrimSuffix(f.DisplayPath(), filepath.Ext(f.DisplayPath()))
			// Match by prefix (handles Movie.en.srt, Movie.es.srt)
			if strings.HasPrefix(fBase, videoBase) || filepath.Dir(f.DisplayPath()) == filepath.Dir(video.DisplayPath()) {
				f.Download()
				totalBytes += f.Length()
				subCount++
			}
		}
	}

	skipped := len(files) - 1 - subCount
	if skipped > 0 {
		log.Printf("[%s] selected: %s (%s) + %d subs, skipped %d files",
			taskID[:8], filepath.Base(fileName), formatBytes(video.Length()), subCount, skipped)
	}

	return totalBytes, fileName
}

func buildMagnet(infoHash string) string {
	params := []string{"xt=urn:btih:" + infoHash}
	for _, tracker := range defaultTrackers {
		params = append(params, "tr="+url.QueryEscape(tracker))
	}
	return "magnet:?" + strings.Join(params, "&")
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %s", float64(b)/float64(div), []string{"KB", "MB", "GB", "TB"}[exp])
}

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
	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/dht/v2/krpc"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/storage"
	"github.com/torrentclaw/torrentclaw-cli/internal/config"
	"golang.org/x/time/rate"
)

var defaultTrackers = []string{
	// Tier 1: ngosang/trackerslist "best" + newtrackon "stable"
	"udp://tracker.opentrackr.org:1337/announce",
	"udp://open.tracker.cl:1337/announce",
	"udp://tracker.openbittorrent.com:6969/announce",
	"udp://tracker.torrent.eu.org:451/announce",
	"udp://open.stealth.si:80/announce",
	"udp://exodus.desync.com:6969/announce",
	"udp://open.demonii.com:1337/announce",
	"udp://tracker.qu.ax:6969/announce",
	"udp://tracker.dler.org:6969/announce",
	"udp://tracker.filemail.com:6969/announce",
	"udp://tracker.theoks.net:6969/announce",
	"udp://tracker.bittor.pw:1337/announce",
	"udp://tracker-udp.gbitt.info:80/announce",
	"udp://open.dstud.io:6969/announce",
	"udp://leet-tracker.moe:1337/announce",
	// Tier 2: newtrackon stable (95%+ uptime)
	"udp://tracker.torrust-demo.com:6969/announce",
	"udp://tracker.plx.im:6969/announce",
	"udp://tracker.tryhackx.org:6969/announce",
	"udp://tracker.fnix.net:6969/announce",
	"udp://tracker.srv00.com:6969/announce",
	"udp://tracker.corpscorp.online:80/announce",
	"udp://tracker.opentorrent.top:6969/announce",
	"udp://tracker.flatuslifir.is:6969/announce",
	"udp://tracker.gmi.gd:6969/announce",
	"udp://tracker.t-1.org:6969/announce",
	"udp://tracker.bluefrog.pw:2710/announce",
	"udp://evan.im:6969/announce",
	// Tier 3: additional coverage
	"udp://t.overflow.biz:6969/announce",
	"udp://wepzone.net:6969/announce",
	"udp://tracker.alaskantf.com:6969/announce",
	"udp://tracker.therarbg.to:6969/announce",
}

// TorrentConfig holds settings for the BitTorrent downloader.
type TorrentConfig struct {
	DataDir          string
	MetadataTimeout  time.Duration // how long to wait for torrent metadata (default 15m, 0 = unlimited)
	StallTimeout     time.Duration // no progress during download for this long = stall (default 10m)
	MaxTimeout       time.Duration // absolute maximum per torrent (default 0 = unlimited)
	MaxDownloadRate  int64         // bytes/s, 0 = unlimited
	MaxUploadRate    int64         // bytes/s, 0 = unlimited
	ListenPort       int           // fixed port for incoming peers (default 42069, 0 = random)
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
	// 0 = unlimited for all timeouts (like qBittorrent)
	// Users can set these in config.toml [downloads] section

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	tcfg := torrent.NewDefaultClientConfig()
	tcfg.DataDir = cfg.DataDir
	tcfg.Seed = cfg.SeedEnabled
	tcfg.NoUpload = !cfg.SeedEnabled
	tcfg.Logger = alog.Default.FilterLevel(alog.Warning)

	// --- Performance optimizations ---

	// Storage: mmap instead of default file backend.
	// The library author notes file storage has "very high system overhead".
	// mmap improves I/O throughput and piece verification speed significantly.
	tcfg.DefaultStorage = storage.NewMMap(cfg.DataDir)

	// Fixed port for incoming peer connections (enables UPnP port mapping).
	// With ListenPort=0, only ~30% of peers can connect to us.
	listenPort := cfg.ListenPort
	if listenPort == 0 {
		listenPort = 42069
	}
	tcfg.ListenPort = listenPort

	// Connection limits: more peers = more download sources.
	// Defaults are conservative (50/25/100). Beyond ~100 established, diminishing returns.
	tcfg.EstablishedConnsPerTorrent = 80
	tcfg.HalfOpenConnsPerTorrent = 50
	tcfg.TotalHalfOpenConns = 150

	// Pipeline depth: bytes downloaded but not yet hash-verified.
	// Default 64 MiB throttles fast connections. The library author recommends
	// "set a very large MaxUnverifiedBytes" for speed (Discussion #741).
	tcfg.MaxUnverifiedBytes = 256 << 20 // 256 MiB

	// Faster peer discovery: default is 10 dials/s which is very conservative.
	tcfg.DialRateLimiter = rate.NewLimiter(40, 40)

	// IPv6 peer selection is poor in anacrolix (Issue #713) — wastes connections.
	tcfg.DisableIPv6 = true

	// Accept incoming connections faster + clean up useless peers.
	tcfg.DisableAcceptRateLimiting = true
	tcfg.DropDuplicatePeerIds = true
	tcfg.DropMutuallyCompletePeers = true

	// --- Rate limiting ---

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

	// --- DHT tuning ---

	// Feed cached nodes into the bootstrap traversal (not just AddDhtNodes post-creation).
	// StartingNodes are used during the initial Bootstrap() which populates the routing table
	// much faster than async pings from AddDhtNodes().
	dhtNodesPath := dhtNodesBinPath()
	tcfg.DhtStartingNodes = func(network string) dht.StartingNodesGetter {
		return func() ([]dht.Addr, error) {
			addrs, _ := dht.GlobalBootstrapAddrs(network)
			// Merge cached nodes from previous session
			cached, err := dht.ReadNodesFromFile(dhtNodesPath)
			if err == nil && len(cached) > 0 {
				for _, ni := range cached {
					addrs = append(addrs, dht.NewAddr(ni.Addr.UDP()))
				}
				log.Printf("[torrent] DHT: loaded %d cached nodes into bootstrap", len(cached))
			}
			return addrs, nil
		}
	}

	// Tune DHT server for faster warmup and more aggressive peer discovery.
	tcfg.ConfigureAnacrolixDhtServer = func(cfg *dht.ServerConfig) {
		// Increase send rate: default 250/s burst 25 is conservative.
		// Higher rate lets bootstrap query more nodes concurrently.
		cfg.SendLimiter = rate.NewLimiter(500, 50)
		// Faster query retries: default 2s, reduce to 1s for quicker fallback.
		cfg.QueryResendDelay = func() time.Duration { return time.Second }
		// Accept all node IDs regardless of BEP 42 validation.
		// Fills routing table faster; most clients don't enforce BEP 42 strictly.
		cfg.NoSecurity = true
		// Request both IPv4 node lists in responses.
		cfg.DefaultWant = []krpc.Want{krpc.WantNodes}
	}

	// Re-announce active torrents to DHT periodically (keeps routing table healthy).
	tcfg.PeriodicallyAnnounceTorrentsToDht = true

	// Try to create client; if the port is in use, try the next few ports.
	var client *torrent.Client
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		client, err = torrent.NewClient(tcfg)
		if err == nil {
			break
		}
		if !strings.Contains(err.Error(), "address already in use") {
			return nil, fmt.Errorf("create torrent client: %w", err)
		}
		tcfg.ListenPort++
		log.Printf("[torrent] port %d in use, trying %d", tcfg.ListenPort-1, tcfg.ListenPort)
	}
	if err != nil {
		return nil, fmt.Errorf("create torrent client (all ports busy): %w", err)
	}
	if tcfg.ListenPort != listenPort {
		log.Printf("[torrent] listening on port %d (configured: %d was busy)", tcfg.ListenPort, listenPort)
	}

	// Restore DHT nodes with full node IDs (direct routing table insertion, no async pings).
	for _, s := range client.DhtServers() {
		if w, ok := s.(torrent.AnacrolixDhtServerWrapper); ok {
			if added, err := w.Server.AddNodesFromFile(dhtNodesPath); err == nil && added > 0 {
				log.Printf("[torrent] DHT: restored %d nodes directly into routing table", added)
			}
		}
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

	// 1. Wait for metadata (0 = unlimited, like qBittorrent)
	if d.cfg.MetadataTimeout > 0 {
		log.Printf("[%s] waiting for metadata (timeout: %s, trackers: %d)...", task.ID[:8], d.cfg.MetadataTimeout, len(defaultTrackers))
	} else {
		log.Printf("[%s] waiting for metadata (no timeout, trackers: %d)...", task.ID[:8], len(defaultTrackers))
	}

	if d.cfg.MetadataTimeout > 0 {
		metaCtx, metaCancel := context.WithTimeout(ctx, d.cfg.MetadataTimeout)
		defer metaCancel()
		select {
		case <-t.GotInfo():
			log.Printf("[%s] metadata received: %s (%d files)", task.ID[:8], t.Name(), len(t.Files()))
		case <-metaCtx.Done():
			stats := t.Stats()
			cleanup()
			return nil, fmt.Errorf("metadata timeout after %s (peers: %d)", d.cfg.MetadataTimeout, stats.ActivePeers)
		}
	} else {
		// Unlimited — wait until metadata arrives or context is cancelled
		select {
		case <-t.GotInfo():
			log.Printf("[%s] metadata received: %s (%d files)", task.ID[:8], t.Name(), len(t.Files()))
		case <-ctx.Done():
			cleanup()
			return nil, fmt.Errorf("cancelled while waiting for metadata")
		}
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

	// MaxTimeout = 0 means unlimited (like qBittorrent)
	var deadline <-chan time.Time
	if d.cfg.MaxTimeout > 0 {
		deadline = time.After(d.cfg.MaxTimeout)
	}
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

			// Stall detection (0 = disabled, like qBittorrent)
			if downloaded > lastBytes {
				lastBytesAt = now
				lastBytes = downloaded
			} else if d.cfg.StallTimeout > 0 && now.Sub(lastBytesAt) > d.cfg.StallTimeout {
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

			// Terminal progress (log.Printf for daemon-friendly output, no \r)
			pct := int(float64(downloaded) / float64(totalBytes) * 100)
			log.Printf("[%s] %d%% — %s/%s @ %s/s  peers:%d seeds:%d",
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
	// Save DHT nodes in binary format for next session (warm start)
	saveDhtNodesBinary(d.client)

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

// SaveDhtNodes persists DHT nodes to disk (for periodic saves from daemon).
func (d *TorrentDownloader) SaveDhtNodes() {
	saveDhtNodesBinary(d.client)
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

// ---------------------------------------------------------------------------
// DHT node persistence — binary format with node IDs for direct table insertion
// ---------------------------------------------------------------------------

const dhtNodesBinFile = "dht-nodes.bin"

// dhtNodesBinPath returns the path to the binary DHT nodes cache file.
func dhtNodesBinPath() string {
	return filepath.Join(config.DataDir(), dhtNodesBinFile)
}

// saveDhtNodesBinary exports known DHT nodes with full node IDs (20-byte ID + address).
// Binary format allows AddNodesFromFile to insert directly into routing table buckets
// without needing async pings, which is much faster than text-based host:port persistence.
func saveDhtNodesBinary(client *torrent.Client) {
	var allNodes []krpc.NodeInfo
	for _, s := range client.DhtServers() {
		if w, ok := s.(torrent.AnacrolixDhtServerWrapper); ok {
			allNodes = append(allNodes, w.Nodes()...)
		}
	}
	if len(allNodes) == 0 {
		return
	}

	path := dhtNodesBinPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}

	if err := dht.WriteNodesToFile(allNodes, path); err != nil {
		log.Printf("[torrent] DHT: error saving nodes: %v", err)
		return
	}
	log.Printf("[torrent] DHT: saved %d nodes to cache", len(allNodes))
}

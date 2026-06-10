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

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/dht/v2/krpc"
	alog "github.com/anacrolix/log"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/storage"
	"github.com/torrentclaw/unarr/internal/agent"
	"github.com/torrentclaw/unarr/internal/config"
	"github.com/torrentclaw/unarr/internal/vpn"
	"golang.org/x/term"
	"golang.org/x/time/rate"
)

// portfwdFilterHandler wraps anacrolix/log handlers and drops the noisy
// UPnP/NAT-PMP port-mapping warnings (e.g. "error: AddPortMapping: 500 Internal
// Server Error") that home routers emit when they reject the mapping. Everything
// else passes through unchanged.
type portfwdFilterHandler struct {
	inner []alog.Handler
}

func (h portfwdFilterHandler) Handle(r alog.Record) {
	if strings.Contains(r.Text(), "AddPortMapping") {
		return
	}
	for _, inner := range h.inner {
		inner.Handle(r)
	}
}

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
	DataDir string
	// PieceCompletionDir, when non-empty, stores the piece-completion SQLite DB
	// in this directory instead of DataDir. Use the agent's local state dir
	// (not the download dir) so the DB never lands on NFS/SMB volumes where
	// SQLite locking times out.
	PieceCompletionDir string
	MetadataTimeout    time.Duration // how long to wait for torrent metadata (default 15m, 0 = unlimited)
	StallTimeout       time.Duration // no progress during download for this long = stall (default 10m)
	MaxTimeout         time.Duration // absolute maximum per torrent (default 0 = unlimited)
	MaxDownloadRate    int64         // bytes/s, 0 = unlimited
	MaxUploadRate      int64         // bytes/s, 0 = unlimited
	ListenPort         int           // fixed port for incoming peers (default 42069, 0 = random)
	SeedEnabled        bool
	SeedRatio          float64       // target seed ratio (default 0, meaning seed until SeedTime)
	SeedTime           time.Duration // min seed time after completion (default 0)

	// VPNTunnel, when set, split-tunnels the torrent client's peer + tracker
	// traffic through an in-process userspace WireGuard tunnel (managed-VPN
	// add-on). nil = downloads in the clear. Brought up by the daemon.
	VPNTunnel *vpn.Tunnel
}

// TorrentDownloader downloads torrents via BitTorrent P2P.
type TorrentDownloader struct {
	client *torrent.Client
	cfg    TorrentConfig

	activeMu sync.Mutex
	active   map[string]*torrent.Torrent // taskID -> torrent handle

	// seedCtx scopes the background seeders. Cancelled at Shutdown so they stop
	// uploading and exit; it must outlive any single download's task context
	// (which is cancelled the moment Download returns and the queue slot frees).
	seedCtx    context.Context
	seedCancel context.CancelFunc
	// seedCheckInterval is how often the background seeder re-checks its stop
	// condition. Defaults to defaultSeedCheckInterval; tests lower it.
	seedCheckInterval time.Duration

	minFreeBytes int64 // disk reserve for the pre-flight space check (0 = reserve disabled)
}

// SetMinFreeBytes sets the free-space reserve enforced before a download starts.
// Call once at construction; 0 disables the reserve (the size-vs-free check still
// runs). See CheckDiskSpace.
func (d *TorrentDownloader) SetMinFreeBytes(n int64) { d.minFreeBytes = n }

// NewTorrentDownloader creates a BitTorrent downloader with a long-lived client.
func NewTorrentDownloader(cfg TorrentConfig) (*TorrentDownloader, error) {
	// MetadataTimeout: 0 = unlimited (wait forever like qBittorrent)
	// StallTimeout: default 30m (no bytes for 30 min = dead torrent, frees the slot)
	if cfg.StallTimeout == 0 {
		cfg.StallTimeout = 30 * time.Minute
	}

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	tcfg := torrent.NewDefaultClientConfig()
	tcfg.DataDir = cfg.DataDir
	tcfg.Seed = cfg.SeedEnabled
	tcfg.NoUpload = !cfg.SeedEnabled
	tcfg.Logger = alog.Default.FilterLevel(alog.Warning)
	// Drop the noisy UPnP/NAT-PMP port-mapping warnings. The library attempts to
	// map the listen port on the router for inbound peers (best-effort, only
	// helps on routers that support it). Many home routers reject AddPortMapping
	// with "500 Internal Server Error" and the lib retries on every lease cycle,
	// spamming the log. The rejection is harmless (download works over DHT +
	// outbound peers), so suppress just that line while keeping the attempts for
	// routers that do support it.
	tcfg.Logger.SetHandlers(portfwdFilterHandler{
		inner: append([]alog.Handler(nil), alog.Default.Handlers...),
	})

	// No browser-facing WebTorrent peer; daemon never seeds via WSS.
	tcfg.DisableWebtorrent = true

	// --- Performance optimizations ---

	// Storage: mmap instead of default file backend.
	// The library author notes file storage has "very high system overhead".
	// mmap improves I/O throughput and piece verification speed significantly.
	//
	// When PieceCompletionDir is set (daemon always passes the agent state dir),
	// keep the piece-completion SQLite DB off the download dir so it never lands
	// on NFS/SMB where SQLite's file locking times out and emits a warning.
	if cfg.PieceCompletionDir != "" {
		if mkErr := os.MkdirAll(cfg.PieceCompletionDir, 0o755); mkErr != nil {
			log.Printf("[torrent] piece-completion dir create failed (%v), DB stays in download dir", mkErr)
			tcfg.DefaultStorage = storage.NewMMap(cfg.DataDir)
		} else if pc, pcErr := storage.NewDefaultPieceCompletionForDir(cfg.PieceCompletionDir); pcErr != nil {
			log.Printf("[torrent] piece-completion db in %q failed (%v), falling back to download dir", cfg.PieceCompletionDir, pcErr)
			tcfg.DefaultStorage = storage.NewMMap(cfg.DataDir)
		} else {
			tcfg.DefaultStorage = storage.NewMMapWithCompletion(cfg.DataDir, pc)
		}
	} else {
		tcfg.DefaultStorage = storage.NewMMap(cfg.DataDir)
	}

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

	// --- Managed-VPN split-tunnel ---
	// Route the torrent client's outbound peer + tracker traffic through the
	// in-process WireGuard tunnel so the swarm + trackers see the VPN IP, not
	// the user's. unarr's control plane keeps using the normal net. uTP (UDP
	// peers) is disabled — TCP peers + HTTP/UDP tracker announces are tunnelled;
	// inbound peers don't apply (leech-only, no port forward).
	if cfg.VPNTunnel != nil {
		tcfg.DisableUTP = true
		tcfg.TrackerDialContext = cfg.VPNTunnel.Net.DialContext
		tcfg.HTTPDialContext = cfg.VPNTunnel.Net.DialContext
		tcfg.TrackerListenPacket = cfg.VPNTunnel.ListenPacket
		log.Printf("[torrent] VPN split-tunnel enabled (peer + tracker traffic routed through WireGuard)")
	}

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

	// Route outgoing peer dials through the VPN tunnel (TCP). Added after client
	// creation; DialForPeerConns defaults to true so this is used for peers.
	if cfg.VPNTunnel != nil {
		client.AddDialer(torrent.NetworkDialer{Network: "tcp", Dialer: cfg.VPNTunnel.Net})
	}

	// Restore DHT nodes with full node IDs (direct routing table insertion, no async pings).
	for _, s := range client.DhtServers() {
		if w, ok := s.(torrent.AnacrolixDhtServerWrapper); ok {
			if added, err := w.Server.AddNodesFromFile(dhtNodesPath); err == nil && added > 0 {
				log.Printf("[torrent] DHT: restored %d nodes directly into routing table", added)
			}
		}
	}

	seedCtx, seedCancel := context.WithCancel(context.Background())
	return &TorrentDownloader{
		client:            client,
		cfg:               cfg,
		active:            make(map[string]*torrent.Torrent),
		seedCtx:           seedCtx,
		seedCancel:        seedCancel,
		seedCheckInterval: defaultSeedCheckInterval,
	}, nil
}

func (d *TorrentDownloader) Method() DownloadMethod { return MethodTorrent }

func (d *TorrentDownloader) Available(_ context.Context, task *Task) (bool, error) {
	return task.InfoHash != "", nil
}

func (d *TorrentDownloader) Download(ctx context.Context, task *Task, outputDir string, progressCh chan<- Progress) (*Result, error) {
	magnet := d.buildMagnet(task.InfoHash)

	t, err := d.client.AddMagnet(magnet)
	if err != nil {
		return nil, fmt.Errorf("add magnet: %w", err)
	}

	// Track active torrent
	d.activeMu.Lock()
	d.active[task.ID] = t
	d.activeMu.Unlock()

	// cleanup drops the torrent and stops tracking it. Used by every error path
	// (metadata timeout, disk guard, poll failure) and by the non-seeding success
	// path — all of which must drop. The seeding success path deliberately does
	// NOT call cleanup (it hands off to seedAndDrop).
	cleanup := func() { d.dropTracked(task.ID, t) }

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

	// 2.5 Pre-flight disk-space guard — refuse before writing rather than fill
	// the disk to 0 mid-download (corrupts the partial file). Torrents land in
	// DataDir (not the manager's outputDir), so stat DataDir. Conservative: uses
	// the full selected size without subtracting pieces a resume may already hold.
	if err := CheckDiskSpace(d.cfg.DataDir, totalBytes, d.minFreeBytes); err != nil {
		cleanup()
		return nil, err
	}

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

	// anacrolix mmap storage (storage.NewMMap) creates completed files with mode
	// 0000 — the running process keeps its own mmap handle so the download works,
	// but any fresh open (streaming, ffprobe/HLS, organize-then-reopen) hits
	// "permission denied". Relax perms now, before organize moves the file, so the
	// readable mode is preserved through the rename.
	makeReadable(filePath)

	// Seeding handoff: with seeding enabled, keep the torrent uploading in the
	// background — seedAndDrop drops it once the ratio/time target is hit (or at
	// shutdown). Otherwise drop now. seedAndDrop must NOT use ctx: the task
	// context is cancelled the moment Download returns and the manager frees the
	// queue slot, which would kill the seeder instantly.
	if d.cfg.SeedEnabled {
		go d.seedAndDrop(task.ID, t, totalBytes)
	} else {
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
	isTTY := term.IsTerminal(int(os.Stderr.Fd()))

	for {
		select {
		case <-ctx.Done():
			if isTTY {
				fmt.Fprintln(os.Stderr)
			}
			return nil, fmt.Errorf("cancelled")

		case <-deadline:
			if isTTY {
				fmt.Fprintln(os.Stderr)
			}
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

			// Terminal progress
			pct := int(float64(downloaded) / float64(totalBytes) * 100)
			line := fmt.Sprintf("[%s] %d%% — %s/%s @ %s/s  peers:%d seeds:%d",
				task.ID[:8], pct,
				formatBytes(downloaded), formatBytes(totalBytes), formatBytes(speed),
				stats.ActivePeers, stats.ConnectedSeeders)
			if isTTY {
				fmt.Fprintf(os.Stderr, "\r\033[K%s", line)
			} else {
				log.Print(line)
			}

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
				if isTTY {
					fmt.Fprintln(os.Stderr) // newline after \r progress
				}
				log.Printf("[%s] download complete: %s", task.ID[:8], fileName)
				return &Result{}, nil
			}
		}
	}
}

// dropTracked stops tracking taskID and drops the torrent handle. The delete is
// guarded on the entry still being this handle, so a concurrent Pause/Cancel that
// already removed/replaced it isn't clobbered; t.Drop() is idempotent. Shared by
// the error/non-seeding cleanup path and the post-seeding drop.
func (d *TorrentDownloader) dropTracked(taskID string, t *torrent.Torrent) {
	d.activeMu.Lock()
	if cur, ok := d.active[taskID]; ok && cur == t {
		delete(d.active, taskID)
	}
	d.activeMu.Unlock()
	t.Drop()
}

// defaultSeedCheckInterval is how often the background seeder re-evaluates the
// ratio / time stop condition. Seeding is long-running and low-urgency, so a
// coarse interval keeps the overhead negligible. Stored on the downloader so
// tests can lower it.
const defaultSeedCheckInterval = 30 * time.Second

// seedTargetReached reports why seeding should stop, or "" to keep going.
// Ratio is uploaded-data / selected-size ("uploaded N× the content"), which is
// stable across resumes — unlike uploaded/downloaded-this-session. The two
// targets are independent: whichever of ratio (>0) or time (>0) fires first
// wins; with both unset nothing ever fires (the caller seeds indefinitely).
func seedTargetReached(ratioTarget float64, timeTarget time.Duration, uploaded, size int64, elapsed time.Duration) string {
	var ratio float64
	if size > 0 {
		ratio = float64(uploaded) / float64(size)
	}
	switch {
	case ratioTarget > 0 && ratio >= ratioTarget:
		return fmt.Sprintf("ratio %.2f reached (target %.2f)", ratio, ratioTarget)
	case timeTarget > 0 && elapsed >= timeTarget:
		return fmt.Sprintf("seed time %s reached (target %s)", elapsed.Round(time.Second), timeTarget)
	}
	return ""
}

// seedAndDrop keeps a completed torrent uploading until the configured ratio or
// time target is reached, then drops it (stops seeding, releases the handle and
// its queue tracking). Runs detached on d.seedCtx — see the Download call site
// for why it can't use the task context. With no ratio/time target it returns
// immediately and the torrent seeds until Shutdown (or a user cancel/pause drops
// it). It exits without dropping if the handle was already removed elsewhere, so
// it never reads stats off a closed torrent nor double-drops.
func (d *TorrentDownloader) seedAndDrop(taskID string, t *torrent.Torrent, totalBytes int64) {
	sid := agent.ShortID(taskID)

	ratioTarget := d.cfg.SeedRatio
	timeTarget := d.cfg.SeedTime
	if ratioTarget <= 0 && timeTarget <= 0 {
		log.Printf("[%s] seeding indefinitely (no ratio/time target) — drops at shutdown", sid)
		return
	}
	log.Printf("[%s] seeding (ratio target: %.2f, time target: %s)", sid, ratioTarget, timeTarget)

	interval := d.seedCheckInterval
	if interval <= 0 {
		interval = defaultSeedCheckInterval
	}
	start := time.Now()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-d.seedCtx.Done():
			return // daemon shutting down — Shutdown drops the handle
		case <-ticker.C:
			// Bail if the handle was dropped elsewhere (user cancel/pause).
			d.activeMu.Lock()
			cur, ok := d.active[taskID]
			d.activeMu.Unlock()
			if !ok || cur != t {
				return
			}

			stats := t.Stats()
			uploaded := stats.BytesWrittenData.Int64()
			reason := seedTargetReached(ratioTarget, timeTarget, uploaded, totalBytes, time.Since(start))
			if reason == "" {
				continue
			}

			log.Printf("[%s] seeding complete: %s, uploaded %s — dropping", sid, reason, formatBytes(uploaded))
			d.dropTracked(taskID, t)
			return
		}
	}
}

// makeReadable relaxes permissions on a completed download so it can be
// re-opened by streaming/ffprobe/organize. anacrolix mmap storage creates
// files with mode 0000; we set files to 0644 and directories to 0755. Best
// effort + non-fatal — but a chmod that fails (typically NFS root_squash / SMB
// uid mapping) is surfaced with a clear, actionable WARNING instead of leaving
// the file 0000 to produce a cryptic "permission denied" later in the pipeline.
func makeReadable(path string) {
	info, err := os.Stat(path)
	if err != nil {
		log.Printf("[organize] makeReadable stat %q: %v", path, err)
		return
	}
	if !info.IsDir() {
		if err := os.Chmod(path, 0o644); err != nil {
			log.Printf("[organize] makeReadable chmod %q: %v", path, err)
		}
		// Verify the file is actually openable now — on NFS/SMB the chmod may
		// "succeed" yet leave it unreadable to this uid. Catch it here with a
		// pointed message rather than as an opaque error at stream/probe time.
		warnIfUnreadable(path)
		return
	}
	var chmodFails int
	var firstFile string
	err = filepath.WalkDir(path, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // skip unreadable entries, keep going
		}
		mode := os.FileMode(0o644)
		if d.IsDir() {
			mode = 0o755
		} else if firstFile == "" {
			firstFile = p
		}
		if err := os.Chmod(p, mode); err != nil {
			chmodFails++
			log.Printf("[organize] makeReadable chmod %q: %v", p, err)
		}
		return nil
	})
	if err != nil {
		log.Printf("[organize] makeReadable walk %q: %v", path, err)
	}
	if chmodFails > 0 {
		log.Printf("[organize] WARNING: %d file(s) under %q could not be made readable (chmod failed) — likely NFS root_squash or an SMB uid mapping. Streaming, ffprobe and organize will fail to open them. Run the agent as the user that owns the share, or mount it so that user can chmod.", chmodFails, path)
	}
	// Same silent-unreadable check the single-file path does: on NFS/SMB a chmod
	// can "succeed" yet leave the file unopenable. Probe one representative file
	// so the directory path catches that case too, not only outright chmod errors.
	if firstFile != "" {
		warnIfUnreadable(firstFile)
	}
}

// warnIfUnreadable logs a clear, actionable warning when a file we just chmod'd
// still can't be opened for reading — the anacrolix-mmap-0000 + NFS/SMB failure
// mode. Best effort: it neither fails the download nor blocks delivery.
func warnIfUnreadable(path string) {
	f, err := os.Open(path)
	if err != nil {
		log.Printf("[organize] WARNING: %q is not readable after chmod (%v) — likely NFS root_squash or an SMB uid mapping (anacrolix mmap creates files mode 0000). Streaming/ffprobe/organize will fail. Run the agent as the user that owns the share, or mount it so that user can chmod.", path, err)
		return
	}
	_ = f.Close()
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
	// Stop background seeders first so they don't read stats off / re-drop the
	// handles we're about to close.
	if d.seedCancel != nil {
		d.seedCancel()
	}

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

// GetStreamProvider returns a FileProvider for the largest video file in an active torrent.
// Used with the persistent StreamServer's SetFile() method.
func (d *TorrentDownloader) GetStreamProvider(taskID string) (FileProvider, error) {
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

	// The provider probes the bitrate asynchronously (to size the streaming
	// readahead) — passing DataDir lets it locate the on-disk file without
	// blocking stream start.
	return NewTorrentFileProvider(video, d.cfg.DataDir), nil
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

// buildMagnet composes a magnet URI for the info hash with the static
// tracker list.
func buildMagnet(infoHash string) string {
	params := []string{"xt=urn:btih:" + infoHash}
	for _, tracker := range defaultTrackers {
		params = append(params, "tr="+url.QueryEscape(tracker))
	}
	return "magnet:?" + strings.Join(params, "&")
}

func (d *TorrentDownloader) buildMagnet(infoHash string) string {
	return buildMagnet(infoHash)
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	// Cap exp at the last unit so an exabyte-scale value (or a corrupt/huge
	// size) can never index past the slice and panic.
	units := []string{"KB", "MB", "GB", "TB", "PB", "EB"}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit && exp < len(units)-1; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %s", float64(b)/float64(div), units[exp])
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

	// Cap at 200 nodes to prevent unbounded file growth
	if len(allNodes) > 200 {
		allNodes = allNodes[:200]
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

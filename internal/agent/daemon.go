package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/torrentclaw/unarr/internal/upgrade"
)

// DaemonConfig holds daemon runtime settings.
type DaemonConfig struct {
	AgentID            string
	AgentName          string
	Version            string
	DownloadDir        string
	StreamPort         int      // port for the HTTP stream server
	StreamSecret       string   // hex HMAC key for stream tokens (reported so the web can mint HLS tokens)
	LanIP              string   // LAN IP (reported in sync for stream URL resolution)
	TailscaleIP        string   // Tailscale IP (reported in sync for stream URL resolution)
	CanDelete          bool     // library.allow_delete is enabled
	ScanPaths          []string // configured scan paths for file deletion validation
	HWAccel            string   // detected encoder backend ("nvenc"/"qsv"/"vaapi"/"videotoolbox"/"none")
	MaxTranscodeHeight int      // resolution cap the agent can transcode comfortably (px)
	// Diagnostic data populated by engine.DetectHWAccelDiagnostic at daemon
	// start. Surfaced in the web "Diagnose transcoder" modal — lets a user
	// see which encoders the ffmpeg binary supports and which devices the
	// host exposes without running `unarr probe-hwaccel`.
	FFmpegVersion string   // first line of `ffmpeg -version`
	FFmpegPath    string   // resolved binary path
	HWEncoders    []string // HW-class encoder names found in `ffmpeg -encoders`
	HWDevices     []string // device files + driver bins detected at probe time
	AutoUpgrade   bool     // honor server-flagged upgrades by downloading + restarting (default: true)
	Downlink      string   // realtime downlink transport: "auto" (SSE+long-poll fallback) | "sse" | "poll"
}

// Daemon manages agent registration and the sync loop.
type Daemon struct {
	cfg    DaemonConfig
	client *Client
	sync   *SyncClient
	state  *LocalState

	// Callbacks — set by cmd/daemon.go before calling Run.
	OnTasksClaimed    func(tasks []Task)
	OnStreamRequested func(req StreamRequest)
	OnStreamSession   func(sess StreamSession)
	OnControlAction   func(action, taskID string, deleteFiles bool)
	GetActiveCount    func() int // returns number of active downloads (wired from manager)
	// OnAgentKeyMinted fires when a register reply carries a freshly-minted
	// per-machine key (the daemon registered with a general/legacy key). cmd
	// persists it so the next start authenticates with the bound agent key —
	// migrating legacy agents and stopping the per-restart re-mint.
	OnAgentKeyMinted func(newKey string)

	// State
	User                UserInfo
	Features            FeatureFlags
	Info                AgentInfo
	State               DaemonState
	lastNotifiedVersion string

	// Managed-VPN split-tunnel state, set by cmd/daemon.go before Run and folded
	// into DaemonState on every write so external tools (`unarr vpn status`) see it.
	vpnActive bool
	vpnMode   string
	vpnServer string

	// CloudFlare Quick Tunnel public URL; folded into DaemonState + heartbeat
	// so the web can prefer it over Tailscale/LAN for in-browser playback.
	funnelURL string

	// Watching tracks whether a user is viewing download progress in the web UI.
	Watching atomic.Bool

	// ScanNow triggers an immediate library scan.
	ScanNow chan struct{}
}

// NewDaemon creates a daemon with an HTTP client for sync-based communication.
func NewDaemon(cfg DaemonConfig, client *Client) *Daemon {
	state := NewLocalState()
	return &Daemon{
		cfg:     cfg,
		client:  client,
		state:   state,
		sync:    NewSyncClient(client, cfg, state),
		ScanNow: make(chan struct{}, 1),
	}
}

// SyncClient returns the sync client for external wiring.
func (d *Daemon) SyncClient() *SyncClient { return d.sync }

// SetVPNState records the managed-VPN split-tunnel state so it's reflected in the
// daemon state file (read by `unarr vpn status`). Call before Run.
func (d *Daemon) SetVPNState(active bool, mode, server string) {
	d.vpnActive = active
	d.vpnMode = mode
	d.vpnServer = server
}

// SetFunnelURL records the CloudFlare Quick Tunnel hostname so it's reflected
// in the daemon state file (read by `unarr funnel status`) and in heartbeat
// requests (so the web prefers it over Tailscale/LAN). Pass "" to clear.
func (d *Daemon) SetFunnelURL(url string) {
	d.funnelURL = url
	d.State.FunnelURL = url
	WriteState(&d.State)
}

// UpdateStreamSecret sets the hex HMAC key reported on register so the web can
// mint HLS stream tokens the agent will accept.
func (d *Daemon) UpdateStreamSecret(secretHex string) {
	d.cfg.StreamSecret = secretHex
	d.sync.cfg.StreamSecret = secretHex
}

// UpdateStreamPort updates the stream port reported in sync requests.
func (d *Daemon) UpdateStreamPort(port int) {
	d.cfg.StreamPort = port
	d.sync.cfg.StreamPort = port
}

// Register registers the agent and fetches user info + features.
// Retries with exponential backoff on transient errors (429, 5xx, network).
func (d *Daemon) Register(ctx context.Context) error {
	req := RegisterRequest{
		AgentID:            d.cfg.AgentID,
		Name:               d.cfg.AgentName,
		OS:                 runtime.GOOS,
		Arch:               runtime.GOARCH,
		Version:            d.cfg.Version,
		DownloadDir:        d.cfg.DownloadDir,
		StreamPort:         d.cfg.StreamPort,
		StreamSecret:       d.cfg.StreamSecret,
		LanIP:              d.cfg.LanIP,
		TailscaleIP:        d.cfg.TailscaleIP,
		HWAccel:            d.cfg.HWAccel,
		MaxTranscodeHeight: d.cfg.MaxTranscodeHeight,
		FFmpegVersion:      d.cfg.FFmpegVersion,
		FFmpegPath:         d.cfg.FFmpegPath,
		HWEncoders:         d.cfg.HWEncoders,
		HWDevices:          d.cfg.HWDevices,
		VPNActive:          d.vpnActive,
		VPNMode:            d.vpnMode,
		VPNServer:          d.vpnServer,
		FunnelURL:          d.funnelURL,
		IsDocker:           RunningInDocker(),
	}
	if free, total, err := DiskInfo(d.cfg.DownloadDir); err == nil {
		req.DiskFreeBytes = free
		req.DiskTotalBytes = total
	}

	const maxRetries = 5
	backoff := 5 * time.Second

	var resp *RegisterResponse
	var err error
	for attempt := range maxRetries {
		resp, err = d.client.Register(ctx, req)
		if err == nil {
			break
		}
		if !isTransientError(err) {
			return fmt.Errorf("register: %w", err)
		}
		log.Printf("Register failed (attempt %d/%d): %v - retrying in %v", attempt+1, maxRetries, err, backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("register: %w", ctx.Err())
		case <-timer.C:
		}
		backoff = min(backoff*2, 60*time.Second)
	}
	if err != nil {
		return fmt.Errorf("register: %w (after %d retries)", err, maxRetries)
	}

	// Registered with a general/legacy key → the server minted a per-machine key.
	// Persist it (cmd wires the callback) so the next start uses the bound key.
	if resp.AgentKey != "" && d.OnAgentKeyMinted != nil {
		d.OnAgentKeyMinted(resp.AgentKey)
	}

	d.User = resp.User
	d.Features = resp.Features
	now := time.Now()
	d.Info = AgentInfo{
		ID:        d.cfg.AgentID,
		Name:      d.cfg.AgentName,
		User:      resp.User,
		Features:  resp.Features,
		StartedAt: now,
	}
	d.State = DaemonState{
		AgentID:     d.cfg.AgentID,
		Status:      "running",
		Version:     d.cfg.Version,
		PID:         os.Getpid(),
		StartedAt:   now,
		MethodStats: make(map[string]int),
		VPNActive:   d.vpnActive,
		VPNMode:     d.vpnMode,
		VPNServer:   d.vpnServer,
		FunnelURL:   d.funnelURL,
	}
	WriteState(&d.State)

	return nil
}

// Run registers the agent and starts the sync loop.
// Blocks until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	// Register
	if err := d.Register(ctx); err != nil {
		return err
	}

	log.Printf("Agent registered: %s (%s) [%s]", d.User.Name, d.User.Email, d.User.Plan)
	log.Printf("Features: torrent=%v debrid=%v usenet=%v", d.Features.Torrent, d.Features.Debrid, d.Features.Usenet)

	// Usenet needs par2 (segment repair) + an extractor (RAR/7z) on the host.
	// Without par2, a single bad segment corrupts the file silently; without
	// an extractor, RAR-packed downloads can't be unpacked. Warn loudly at
	// startup so the operator installs them before the first download fails.
	if d.Features.Usenet {
		if _, err := exec.LookPath("par2"); err != nil {
			log.Printf("[usenet] WARNING: par2 not found in PATH — corrupted segments cannot be repaired and extraction may fail. Install par2 (apt install par2 / brew install par2).")
		}
		_, unrarErr := exec.LookPath("unrar")
		_, sevenZErr := exec.LookPath("7z")
		if unrarErr != nil && sevenZErr != nil {
			log.Printf("[usenet] WARNING: no archive extractor (unrar or 7z) found — RAR-packed downloads cannot be unpacked. Install unrar or 7z.")
		}
	}

	// Wire sync callbacks
	d.sync.OnNewTasks = func(tasks []Task) {
		if d.OnTasksClaimed != nil {
			d.OnTasksClaimed(tasks)
		}
	}
	d.sync.OnControl = func(action, taskID string, deleteFiles bool) {
		if d.OnControlAction != nil {
			d.OnControlAction(action, taskID, deleteFiles)
		}
	}
	d.sync.OnStreamRequest = func(req StreamRequest) {
		// Off the sync loop: the handler does blocking I/O (os.Stat retries on
		// NFS, then ffprobe in SetFile) — running it inline would stall task
		// dispatch + status reporting for other items. The single-stream model
		// (atomic SetFile swap, last-wins) tolerates concurrent requests.
		if d.OnStreamRequested != nil {
			go d.OnStreamRequested(req)
		}
	}
	d.sync.OnStreamSession = func(sess StreamSession) {
		if d.OnStreamSession != nil {
			d.OnStreamSession(sess)
		}
	}
	d.sync.OnUpgrade = func(version string) {
		if version == d.lastNotifiedVersion {
			return
		}
		d.lastNotifiedVersion = version
		if !d.cfg.AutoUpgrade {
			log.Printf("[upgrade] new version available: %s — auto_upgrade=false, run `unarr update` to apply", version)
			return
		}
		log.Printf("[upgrade] new version available: %s — applying auto-upgrade", version)
		go d.applyAutoUpgrade(version)
	}
	d.sync.OnScan = func() {
		log.Printf("Library scan requested by server")
		select {
		case d.ScanNow <- struct{}{}:
		default:
		}
	}
	d.sync.OnWatchingChange = func(watching bool) {
		d.Watching.Store(watching)
	}
	d.sync.GetVPNState = func() (bool, string, string) {
		return d.vpnActive, d.vpnMode, d.vpnServer
	}
	d.sync.GetFunnelURL = func() string {
		return d.funnelURL
	}
	d.sync.OnSyncSuccess = func() {
		d.State.LastHeartbeat = time.Now()
		if d.GetActiveCount != nil {
			d.State.ActiveTasks = d.GetActiveCount()
		}
		WriteState(&d.State)
	}

	// Start sync loop (blocks)
	return d.sync.Run(ctx)
}

// TriggerSync requests an immediate sync cycle.
func (d *Daemon) TriggerSync() {
	d.sync.TriggerSync()
}

// Deregister notifies the server of graceful shutdown.
func (d *Daemon) Deregister() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.client.Deregister(ctx, d.cfg.AgentID); err != nil {
		log.Printf("Deregister failed: %v", err)
	} else {
		log.Println("Agent deregistered")
	}
	RemoveState()
}

// applyAutoUpgrade downloads the target version and exits so the service
// supervisor (systemd Restart=always on Linux) respawns on the new binary.
// Triggered by the server's upgrade signal — opt-in flag set by the user from
// the web UI; the daemon never auto-upgrades on a passive version bump.
//
// Reports the outcome to /api/internal/agent/upgrade-result so the server
// clears `upgrade_requested`. Without this report the flag stays sticky and
// the daemon would loop on every sync — including the no-op case where it's
// already on the target version.
func (d *Daemon) applyAutoUpgrade(targetVersion string) {
	currentClean := strings.TrimPrefix(d.cfg.Version, "v")
	targetClean := strings.TrimPrefix(targetVersion, "v")

	// No-op: server signal arrived but we're already running the target. This
	// happens when the daemon restarts after a previous auto-upgrade before
	// reportUpgradeResult cleared the flag, or when the operator manually
	// installed the same version off-band. Skip Execute (which would also
	// no-op) AND skip os.Exit, but DO clear the flag — otherwise we loop.
	if currentClean == targetClean {
		log.Printf("[upgrade] already on v%s — clearing server flag", currentClean)
		ctxR, cancelR := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelR()
		if err := d.client.ReportUpgradeResult(ctxR, d.cfg.AgentID, true, currentClean, ""); err != nil {
			log.Printf("[upgrade] report-result failed (will retry on next signal): %v", err)
		}
		return
	}

	upgrader := &upgrade.Upgrader{
		CurrentVersion: currentClean,
		OnProgress: func(msg string) {
			log.Printf("[upgrade] %s", msg)
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	result := upgrader.Execute(ctx, targetVersion)
	if !result.Success {
		log.Printf("[upgrade] auto-upgrade failed: %v", result.Error)
		errMsg := ""
		if result.Error != nil {
			errMsg = result.Error.Error()
		}
		ctxR, cancelR := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelR()
		if err := d.client.ReportUpgradeResult(ctxR, d.cfg.AgentID, false, targetClean, errMsg); err != nil {
			log.Printf("[upgrade] report-result failed: %v", err)
		}
		return
	}
	log.Printf("[upgrade] upgraded v%s → v%s; reporting result + exiting so service supervisor restarts on new binary",
		result.OldVersion, result.NewVersion)
	ctxR, cancelR := context.WithTimeout(context.Background(), 10*time.Second)
	if err := d.client.ReportUpgradeResult(ctxR, d.cfg.AgentID, true, result.NewVersion, ""); err != nil {
		log.Printf("[upgrade] report-result failed: %v", err)
	}
	cancelR()
	time.Sleep(500 * time.Millisecond)
	os.Exit(0)
}

// isTransientError returns true for errors worth retrying (429, 5xx, network).
func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == 429 || httpErr.StatusCode >= 500
	}
	lower := strings.ToLower(err.Error())
	for _, keyword := range []string{"connection refused", "no such host", "timeout", "request failed"} {
		if strings.Contains(lower, keyword) {
			return true
		}
	}
	return false
}

package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/upgrade"
)

// DaemonConfig holds daemon runtime settings.
type DaemonConfig struct {
	AgentID            string
	AgentName          string
	Version            string
	DownloadDir        string
	StreamPort         int      // port for the HTTP stream server
	HTTPSStreamPort    int      // TLS stream listener port (per-agent direct-TLS); 0 when off
	AgentHash          string   // stable high-entropy hash for *.<hash>.agent.unarr.app
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
	// PreferredMethods is the ordered download-method preference from config.toml
	// (e.g. ["debrid","usenet"]). Reported to the web so it honours the gating.
	PreferredMethods []string
	// MaxStreamSessions is the concurrent-HLS-session cap (config.toml
	// downloads.max_stream_sessions). Reported to the web so it never mints more
	// concurrent streams than this agent can hold.
	MaxStreamSessions int
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
	// GetActiveStreamCount returns the number of live stream sessions (player +
	// HLS transcode). Wired from cmd. The graceful AUTO-upgrade path defers
	// while this is > 0 so it never cuts a viewer mid-playback; a MANUAL
	// `unarr update` ignores it and applies immediately.
	GetActiveStreamCount func() int
	// OnAgentKeyMinted fires when a register reply carries a freshly-minted
	// per-machine key (the daemon registered with a general/legacy key). cmd
	// persists it so the next start authenticates with the bound agent key —
	// migrating legacy agents and stopping the per-restart re-mint.
	OnAgentKeyMinted func(newKey string)
	// OnBlocked fires once when a terminal failure parks the daemon, so cmd can
	// tell the user on a channel they will actually see (a desktop notification
	// — the daemon usually runs as a service where stdout goes nowhere).
	OnBlocked func(b *Blocked)
	// ReloadCredential re-reads the API key from disk while blocked. Signing in
	// from the tray rewrites it, and without this the daemon would keep offering
	// the rejected key forever — making a successful sign-in look like a failure.
	ReloadCredential func()
	// OnCredentialRejected fires when the server tombstones this agent, so cmd
	// can wipe the dead credential. Kept separate from OnBlocked: a rejected
	// 401 is ambiguous (a deploy blip rejects a perfectly good key) and must
	// never wipe anything, while a 410 is the server saying this identity is
	// gone for good.
	OnCredentialRejected func()

	// State
	User                UserInfo
	Features            FeatureFlags
	Info                AgentInfo
	State               DaemonState
	lastNotifiedVersion string
	// upgradeDeferring guards a single defer-until-idle waiter for auto-upgrade.
	upgradeDeferring atomic.Bool

	// Managed-VPN split-tunnel state, set by cmd/daemon.go before Run and folded
	// into DaemonState on every write so external tools (`unarr vpn status`) see it.
	// vpnMu guards these because the reconnect supervisor can flip vpnActive live
	// (tunnel down → up) from its own goroutine while the sync loop reads them.
	vpnMu       sync.Mutex
	vpnActive   bool
	vpnRequired bool // config [downloads.vpn] required — the fail-closed P2P kill-switch
	vpnMode     string
	vpnServer   string

	// CloudFlare Quick Tunnel public URL; folded into DaemonState + heartbeat
	// so the web can prefer it over Tailscale/LAN for in-browser playback.
	funnelURL string

	// httpsWanMapped: the stream server auto-published its HTTPS port to the WAN
	// via UPnP (external port matches). Written by the stream server's mapping
	// maintainer goroutine (SetWanMappedCallback → SetHTTPSWanMapped) and read by
	// the sync loop — atomic to avoid a data race across those goroutines.
	httpsWanMapped atomic.Bool

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

// Client exposes the API client so cmd can swap in a credential the user
// re-authorized while the daemon was parked on a rejected one.
func (d *Daemon) Client() *Client { return d.client }

// SetVPNState + vpnSnapshot — the managed-VPN split-tunnel / P2P kill-switch state
// accessors — live in daemon_vpn.go to keep this file within the size budget.

// SetFunnelURL records the CloudFlare Quick Tunnel hostname so it's reflected
// in the daemon state file (read by `unarr funnel status`) and in heartbeat
// requests (so the web prefers it over Tailscale/LAN). Pass "" to clear.
func (d *Daemon) SetFunnelURL(url string) {
	d.funnelURL = url
	d.State.FunnelURL = url
	WriteState(&d.State)
}

// SetHTTPSWanMapped records whether the stream server's HTTPS port is currently
// published to the WAN via UPnP, so the sync heartbeat carries it to the web.
// Fired (on change only) by the stream server's mapping maintainer.
func (d *Daemon) SetHTTPSWanMapped(mapped bool) {
	d.httpsWanMapped.Store(mapped)
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

// UpdateHTTPSStreamPort updates the ACTUAL bound HTTPS stream port reported in
// register/sync, so the web encodes the direct-TLS host on the port the agent is
// really listening on. listenTLS bumps the port when the configured one is busy,
// and it stays 0 when no cert was issued — reporting the config value in either
// case points every direct-TLS URL at a port nothing is serving. 0 is the honest
// answer there, and directTLSWire already means "clear the columns" by it.
func (d *Daemon) UpdateHTTPSStreamPort(port int) {
	d.cfg.HTTPSStreamPort = port
	d.sync.cfg.HTTPSStreamPort = port
}

// directTLSWire renders the per-agent direct-TLS pair for the wire. The daemon
// read the config, so it always reports explicitly: &0/&"" when the feature is
// off, which tells the server to clear the columns instead of keeping a port
// that no longer listens and a hash that squats the unique index.
func directTLSWire(port int, hash string) (*int, *string) {
	return &port, &hash
}

// ApplyReloadedConfig applies the settings a SIGUSR1 reload can change without a
// restart, and reports honestly on the ones it can't. A reload that silently
// no-ops is worse than one that refuses, because the user believes it.
//
// allow_delete rides the next sync (seconds), so it lands immediately.
//
// The method order does NOT: it is only ever sent at register time, and
// re-registering from here is not safe — Register rewrites d.State (raced by the
// sync/VPN goroutines) and can fire OnAgentKeyMinted, whose config.Save writes
// the STARTUP config back to disk, clobbering the very edit that triggered this
// reload. So we say it needs a restart instead of pretending. Restart is a real
// fix now: the daemon reports "auto" explicitly, so the server stops enforcing a
// stale order (it never did before — see RegisterRequest.PreferredMethods).
func (d *Daemon) ApplyReloadedConfig(allowDelete bool, methodOrder []string) {
	if d.sync == nil {
		return
	}
	// Never advertise a capability we can't perform: without a delete handler
	// (no scan paths at startup) the server would queue deletions we drop on the
	// floor, and the web would spin on them forever.
	if allowDelete && d.sync.OnDeleteFiles == nil {
		log.Printf("[reload] allow_delete=true ignored: this daemon has no library scan paths")
		allowDelete = false
	}
	d.sync.SetCanDelete(allowDelete)
	log.Printf("[reload] allow_delete=%v (reported on next sync)", allowDelete)

	if methodOrder == nil {
		methodOrder = []string{}
	}
	prev := d.cfg.PreferredMethods
	if prev == nil {
		prev = []string{}
	}
	if !slices.Equal(prev, methodOrder) {
		log.Printf("[reload] preferred method order changed (%v → %v) — RESTART REQUIRED: run 'unarr daemon restart' to apply it", prev, methodOrder)
	}
}

// Register registers the agent and fetches user info + features.
// Retries with exponential backoff on transient errors (429, 5xx, network).
func (d *Daemon) Register(ctx context.Context) error {
	vpnActive, vpnRequired, vpnMode, vpnServer := d.vpnSnapshot()
	// The daemon read config.toml, so it always reports an explicit preference.
	// nil means "auto" locally, but on the wire it must be [] — a missing key
	// leaves a stale server-side list in place (see RegisterRequest).
	methods := d.cfg.PreferredMethods
	if methods == nil {
		methods = []string{}
	}
	httpsPort, agentHash := directTLSWire(d.cfg.HTTPSStreamPort, d.cfg.AgentHash)
	req := RegisterRequest{
		AgentID:            d.cfg.AgentID,
		Name:               d.cfg.AgentName,
		OS:                 runtime.GOOS,
		Arch:               runtime.GOARCH,
		Version:            d.cfg.Version,
		DownloadDir:        d.cfg.DownloadDir,
		StreamPort:         d.cfg.StreamPort,
		HTTPSStreamPort:    httpsPort,
		AgentHash:          agentHash,
		StreamSecret:       d.cfg.StreamSecret,
		LanIP:              d.cfg.LanIP,
		TailscaleIP:        d.cfg.TailscaleIP,
		HWAccel:            d.cfg.HWAccel,
		MaxTranscodeHeight: d.cfg.MaxTranscodeHeight,
		FFmpegVersion:      d.cfg.FFmpegVersion,
		FFmpegPath:         d.cfg.FFmpegPath,
		HWEncoders:         d.cfg.HWEncoders,
		HWDevices:          d.cfg.HWDevices,
		VPNActive:          vpnActive,
		VPNMode:            vpnMode,
		VPNServer:          vpnServer,
		FunnelURL:          d.funnelURL,
		IsDocker:           RunningInDocker(),
		PreferredMethods:   &methods,
		MaxStreamSessions:  d.cfg.MaxStreamSessions,
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
		if b, terminal := Classify(err); terminal {
			// Retrying on our own cannot fix this — the user has to. Exiting
			// cannot fix it either: the supervisor is Restart=always, so a
			// process that quits (with ANY status) is back in ten seconds to
			// fail identically, which is how a rejected key became an invisible
			// restart loop. So the daemon stays up, says what is wrong, and
			// waits for the user to resolve it.
			b.Version = d.cfg.Version
			resp, err = d.waitOutBlock(ctx, b, req)
			if err != nil {
				return err
			}
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

	// Registration succeeded, so whatever the user was blocked on is resolved.
	// Clearing it here — rather than where each block is fixed — means a stale
	// block can never outlive the problem: a user who signs in again must not
	// still be told to sign in.
	ClearBlocked()

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
		VPNActive:   vpnActive,
		VPNMode:     vpnMode,
		VPNServer:   vpnServer,
		VPNRequired: vpnRequired,
		VPNBlocking: vpnRequired && !vpnActive,
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
		go d.deferAutoUpgradeUntilIdle(version)
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
		active, _, mode, server := d.vpnSnapshot()
		return active, mode, server
	}
	d.sync.GetFunnelURL = func() string {
		return d.funnelURL
	}
	d.sync.GetHTTPSWanMapped = func() bool {
		return d.httpsWanMapped.Load()
	}
	d.sync.GetAgentStatus = func() string {
		return d.State.Status
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

// deferAutoUpgradeUntilIdle holds an AUTO-upgrade until the agent is idle (no
// active stream), then applies it. The user's call: no background update is
// worth cutting a viewer mid-playback. A MANUAL `unarr update` bypasses this
// entirely (see cmd/self_update.go) and is the escape hatch for an urgent fix.
//
// Runs in its own goroutine. A process-lifetime guard keeps exactly ONE waiter
// even though the server re-sends the upgrade signal on every sync.
func (d *Daemon) deferAutoUpgradeUntilIdle(version string) {
	if !d.upgradeDeferring.CompareAndSwap(false, true) {
		return
	}
	defer d.upgradeDeferring.Store(false)

	activeStreams := func() int {
		if d.GetActiveStreamCount == nil {
			return 0
		}
		return d.GetActiveStreamCount()
	}

	if n := activeStreams(); n > 0 {
		log.Printf("[upgrade] v%s deferred — %d active stream(s); will apply when idle", version, n)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if n := activeStreams(); n == 0 {
				break
			}
		}
		log.Printf("[upgrade] no active streams — applying deferred upgrade to v%s", version)
	}
	d.applyAutoUpgrade(version) // exits the process on success
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

	// Tell the web we're updating so a NEW playback attempt during the brief
	// restart sees "agent updating" instead of a hard session error. One
	// heartbeat carries this before the (blocking) download + os.Exit below.
	d.State.Status = "updating"
	WriteState(&d.State)
	d.TriggerSync()

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
	// Fleet desktops ride the SAME web-triggered signal: `unarr update` in a
	// terminal refreshes the tray companion next to the CLI, so the daemon's
	// auto-upgrade must too — without this, web-managed installs kept their
	// unarr-desktop stale forever (nothing else ever updated it). Reuses the
	// Execute ctx (10-min budget, mostly unspent — the CLI download already
	// happened). Best-effort by design: a sibling failure must NEVER fail the
	// daemon upgrade that already succeeded, and the version marker makes the
	// already-current case a no-op without downloading (or executing)
	// anything. CLI-only installs (docker/servers) have no sibling → silent.
	if updated, derr := upgrade.UpdateDesktopSibling(ctx, result.NewVersion, func(msg string) {
		log.Printf("[upgrade] desktop: %s", msg)
	}); derr != nil {
		if errors.Is(derr, upgrade.ErrNoDesktopAssets) {
			log.Printf("[upgrade] desktop sibling skipped: release v%s ships no signed desktop assets (yet) — it will refresh on a later update", result.NewVersion)
		} else {
			log.Printf("[upgrade] desktop sibling update failed (CLI upgrade unaffected): %v", derr)
		}
	} else if updated {
		log.Printf("[upgrade] desktop sibling updated to v%s (a running tray loads it on its next restart)", result.NewVersion)
	}
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

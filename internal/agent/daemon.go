package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

// DaemonConfig holds daemon runtime settings.
type DaemonConfig struct {
	AgentID     string
	AgentName   string
	Version     string
	DownloadDir string
	StreamPort  int      // port for the HTTP stream server
	LanIP       string   // LAN IP (reported in sync for stream URL resolution)
	TailscaleIP string   // Tailscale IP (reported in sync for stream URL resolution)
	CanDelete   bool     // library.allow_delete is enabled
	ScanPaths   []string // configured scan paths for file deletion validation
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
	OnControlAction   func(action, taskID string, deleteFiles bool)
	GetActiveCount    func() int // returns number of active downloads (wired from manager)

	// State
	User                UserInfo
	Features            FeatureFlags
	Info                AgentInfo
	State               DaemonState
	lastNotifiedVersion string

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

// UpdateStreamPort updates the stream port reported in sync requests.
func (d *Daemon) UpdateStreamPort(port int) {
	d.cfg.StreamPort = port
	d.sync.cfg.StreamPort = port
}

// Register registers the agent and fetches user info + features.
// Retries with exponential backoff on transient errors (429, 5xx, network).
func (d *Daemon) Register(ctx context.Context) error {
	req := RegisterRequest{
		AgentID:     d.cfg.AgentID,
		Name:        d.cfg.AgentName,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		Version:     d.cfg.Version,
		DownloadDir: d.cfg.DownloadDir,
		StreamPort:  d.cfg.StreamPort,
		LanIP:       d.cfg.LanIP,
		TailscaleIP: d.cfg.TailscaleIP,
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
		if d.OnStreamRequested != nil {
			d.OnStreamRequested(req)
		}
	}
	d.sync.OnUpgrade = func(version string) {
		if version != d.lastNotifiedVersion {
			d.lastNotifiedVersion = version
			log.Printf("New version available: %s (run `unarr self-update` to upgrade)", version)
		}
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

package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"runtime"
	"time"
)

// DaemonConfig holds daemon runtime settings.
type DaemonConfig struct {
	AgentID           string
	AgentName         string
	Version           string
	DownloadDir       string
	PollInterval      time.Duration
	HeartbeatInterval time.Duration
}

// Daemon manages the main loop: register, heartbeat, poll tasks.
type Daemon struct {
	cfg       DaemonConfig
	transport Transport

	// Callbacks
	OnTasksClaimed      func(tasks []Task)
	OnStreamRequested   func(req StreamRequest)
	OnUpgradeRequested  func(version string)
	OnControlAction     func(action, taskID string)

	// State
	User              UserInfo
	Features          FeatureFlags
	Info              AgentInfo
	State             DaemonState
	upgradeInProgress bool
	heartbeatFailures int

	// Callbacks for state tracking (set by cmd/daemon.go)
	GetActiveCount func() int

	// Exposed tickers for hot-reload
	PollTicker      *time.Ticker
	HeartbeatTicker *time.Ticker
}

// NewDaemon creates a daemon with the given transport.
// Use NewHTTPTransport for HTTP-only, or NewHybridTransport for WS+HTTP.
func NewDaemon(cfg DaemonConfig, transport Transport) *Daemon {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 30 * time.Second
	}
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = 30 * time.Second
	}

	return &Daemon{
		cfg:       cfg,
		transport: transport,
	}
}

// Transport returns the configured transport.
func (d *Daemon) Transport() Transport { return d.transport }

// Register registers the agent and fetches user info + features.
func (d *Daemon) Register(ctx context.Context) error {
	req := RegisterRequest{
		AgentID:     d.cfg.AgentID,
		Name:        d.cfg.AgentName,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		Version:     d.cfg.Version,
		DownloadDir: d.cfg.DownloadDir,
	}
	if free, total, err := DiskInfo(d.cfg.DownloadDir); err == nil {
		req.DiskFreeBytes = free
		req.DiskTotalBytes = total
	}

	resp, err := d.transport.Register(ctx, req)
	if err != nil {
		return fmt.Errorf("register: %w", err)
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

// Run starts the main daemon loop. Blocks until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	// Register
	if err := d.Register(ctx); err != nil {
		return err
	}

	log.Printf("Agent registered: %s (%s) [%s]", d.User.Name, d.User.Email, d.User.Plan)
	log.Printf("Features: torrent=%v debrid=%v usenet=%v", d.Features.Torrent, d.Features.Debrid, d.Features.Usenet)
	log.Printf("Polling every %s, heartbeat every %s", d.cfg.PollInterval, d.cfg.HeartbeatInterval)

	d.HeartbeatTicker = time.NewTicker(d.cfg.HeartbeatInterval)
	defer d.HeartbeatTicker.Stop()

	d.PollTicker = time.NewTicker(d.cfg.PollInterval)
	defer d.PollTicker.Stop()

	heartbeatTicker := d.HeartbeatTicker
	pollTicker := d.PollTicker

	// Initial poll immediately
	d.poll(ctx)

	eventsCh := d.transport.Events()

	for {
		select {
		case <-ctx.Done():
			log.Println("Daemon shutting down...")
			d.deregister()
			return nil

		case event := <-eventsCh:
			d.handleEvent(event)

		case <-heartbeatTicker.C:
			d.heartbeat(ctx)

		case <-pollTicker.C:
			// Only poll in HTTP mode — WS mode receives tasks via Events
			if d.transport.Mode() == "http" {
				d.poll(ctx)
			}
		}
	}
}

func (d *Daemon) heartbeat(ctx context.Context) {
	req := HeartbeatRequest{
		AgentID:     d.cfg.AgentID,
		Name:        d.cfg.AgentName,
		Version:     d.cfg.Version,
		OS:          runtime.GOOS,
		DownloadDir: d.cfg.DownloadDir,
	}
	if free, total, err := DiskInfo(d.cfg.DownloadDir); err == nil {
		req.DiskFreeBytes = free
		req.DiskTotalBytes = total
	}

	resp, err := d.transport.SendHeartbeat(ctx, req)
	if err != nil {
		d.heartbeatFailures++
		if d.heartbeatFailures >= 5 && d.heartbeatFailures%5 == 0 {
			log.Printf("CRITICAL: %d consecutive heartbeat failures — server may be unreachable", d.heartbeatFailures)
		} else {
			log.Printf("Heartbeat failed: %v", err)
		}
		return
	}
	if d.heartbeatFailures > 0 {
		log.Printf("Heartbeat recovered after %d failures", d.heartbeatFailures)
		d.heartbeatFailures = 0
	}

	// Update state file
	d.State.LastHeartbeat = time.Now()
	if d.GetActiveCount != nil {
		d.State.ActiveTasks = d.GetActiveCount()
	}
	WriteState(&d.State)

	// Check for upgrade signal from server
	if resp.Upgrade != nil && resp.Upgrade.Version != "" && !d.upgradeInProgress {
		d.upgradeInProgress = true
		log.Printf("Upgrade requested by server: %s → %s", d.cfg.Version, resp.Upgrade.Version)
		if d.OnUpgradeRequested != nil {
			go d.OnUpgradeRequested(resp.Upgrade.Version)
		}
	}
}

// handleEvent processes a server-initiated event from the WebSocket transport.
func (d *Daemon) handleEvent(event ServerEvent) {
	switch event.Type {
	case "tasks":
		if event.Tasks != nil && len(event.Tasks.Tasks) > 0 {
			log.Printf("Received %d task(s) via WebSocket", len(event.Tasks.Tasks))
			if d.OnTasksClaimed != nil {
				d.OnTasksClaimed(event.Tasks.Tasks)
			}
		}
		if event.Tasks != nil && d.OnStreamRequested != nil {
			for _, sr := range event.Tasks.StreamRequests {
				d.OnStreamRequested(sr)
			}
		}

	case "upgrade":
		if event.Upgrade != nil && event.Upgrade.Version != "" && !d.upgradeInProgress {
			d.upgradeInProgress = true
			log.Printf("Upgrade requested via WebSocket: %s → %s", d.cfg.Version, event.Upgrade.Version)
			if d.OnUpgradeRequested != nil {
				go d.OnUpgradeRequested(event.Upgrade.Version)
			}
		}

	case "control":
		if event.Control != nil && d.OnControlAction != nil {
			log.Printf("Control action via WebSocket: %s task %s", event.Control.Action, event.Control.TaskID)
			d.OnControlAction(event.Control.Action, event.Control.TaskID)
		}

	case "disconnected":
		log.Println("WebSocket disconnected, switching to HTTP polling")
	}
}

// ClearUpgradeInProgress resets the upgrade flag so a retry can be attempted.
func (d *Daemon) ClearUpgradeInProgress() {
	d.upgradeInProgress = false
}

func (d *Daemon) deregister() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := d.transport.Deregister(ctx, d.cfg.AgentID)
	if err != nil {
		log.Printf("Deregister failed: %v", err)
	} else {
		log.Println("Agent deregistered")
	}
	RemoveState()
}

func (d *Daemon) poll(ctx context.Context) {
	resp, err := d.transport.ClaimTasks(ctx, d.cfg.AgentID)
	if err != nil {
		log.Printf("Poll failed: %v", err)
		return
	}

	d.Info.LastPollAt = time.Now()

	if len(resp.Tasks) > 0 {
		log.Printf("Claimed %d task(s)", len(resp.Tasks))
		if d.OnTasksClaimed != nil {
			d.OnTasksClaimed(resp.Tasks)
		}
	}

	// Handle stream requests for completed downloads
	if d.OnStreamRequested != nil {
		for _, sr := range resp.StreamRequests {
			d.OnStreamRequested(sr)
		}
	}
}

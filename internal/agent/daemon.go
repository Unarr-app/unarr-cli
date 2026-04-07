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
	OnTasksClaimed    func(tasks []Task)
	OnStreamRequested func(req StreamRequest)
	OnControlAction   func(action, taskID string)

	// State
	User                UserInfo
	Features            FeatureFlags
	Info                AgentInfo
	State               DaemonState
	heartbeatFailures   int
	lastNotifiedVersion string

	// Callbacks for state tracking (set by cmd/daemon.go)
	GetActiveCount    func() int
	GetCleanableBytes func() int64

	// Watching tracks whether a user is viewing download progress in the web UI.
	// When false, the progress reporter skips detailed updates (only sends final states).
	// Accessed from heartbeat goroutine, flush goroutine, and WatchingFunc closure — must be atomic.
	Watching atomic.Bool

	// Exposed tickers for hot-reload
	PollTicker      *time.Ticker
	HeartbeatTicker *time.Ticker

	// pollNow triggers an immediate poll (e.g. on resume)
	pollNow chan struct{}

	// ScanNow triggers an immediate library scan (from heartbeat or WebSocket control event)
	ScanNow chan struct{}
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
		pollNow:   make(chan struct{}, 1),
		ScanNow:   make(chan struct{}, 1),
	}
}

// Transport returns the configured transport.
func (d *Daemon) Transport() Transport { return d.transport }

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
		resp, err = d.transport.Register(ctx, req)
		if err == nil {
			break
		}
		// Only retry on transient errors (429, 5xx, network failures)
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

// Run connects the transport, registers the agent, and starts the main loop.
// Blocks until ctx is cancelled. Callers must NOT call transport.Connect before Run.
func (d *Daemon) Run(ctx context.Context) error {
	// Connect transport (establishes WebSocket if available, falls back to HTTP)
	if err := d.transport.Connect(ctx); err != nil {
		return fmt.Errorf("connect transport: %w", err)
	}

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

		case <-d.pollNow:
			d.poll(ctx)
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

	// Update watching flag and state file
	d.Watching.Store(resp.Watching)
	d.State.LastHeartbeat = time.Now()
	if d.GetActiveCount != nil {
		d.State.ActiveTasks = d.GetActiveCount()
	}
	WriteState(&d.State)

	// Trigger library scan if requested
	if resp.Scan {
		log.Printf("Library scan requested by server")
		select {
		case d.ScanNow <- struct{}{}:
		default: // scan already pending
		}
	}

	// Log once per version when server suggests an upgrade
	if resp.Upgrade != nil && resp.Upgrade.Version != "" && resp.Upgrade.Version != d.lastNotifiedVersion {
		d.lastNotifiedVersion = resp.Upgrade.Version
		log.Printf("New version available: %s (run `unarr self-update` to upgrade)", resp.Upgrade.Version)
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
		if event.Upgrade != nil && event.Upgrade.Version != "" && event.Upgrade.Version != d.lastNotifiedVersion {
			d.lastNotifiedVersion = event.Upgrade.Version
			log.Printf("New version available: %s (run `unarr self-update` to upgrade)", event.Upgrade.Version)
		}

	case "control":
		if event.Control != nil {
			log.Printf("Control action via WebSocket: %s task %s", event.Control.Action, event.Control.TaskID)
			if event.Control.Action == "scan" {
				select {
				case d.ScanNow <- struct{}{}:
				default:
				}
			}
			if d.OnControlAction != nil {
				d.OnControlAction(event.Control.Action, event.Control.TaskID)
			}
		}

	case "disconnected":
		log.Println("WebSocket disconnected, switching to HTTP polling")
	}
}

// TriggerPoll requests an immediate task poll cycle.
// Used when a resume event is received to pick up re-pending tasks faster.
func (d *Daemon) TriggerPoll() {
	select {
	case d.pollNow <- struct{}{}:
	default: // already pending
	}
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

// isTransientError returns true for errors worth retrying (429, 5xx, network).
func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	// Structured check: HTTPError carries the status code directly
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == 429 || httpErr.StatusCode >= 500
	}
	// Fallback: network-level errors (no HTTP response received)
	lower := strings.ToLower(err.Error())
	for _, keyword := range []string{"connection refused", "no such host", "timeout", "request failed"} {
		if strings.Contains(lower, keyword) {
			return true
		}
	}
	return false
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

package agent

import (
	"context"
	"fmt"
	"log"
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
	cfg    DaemonConfig
	client *Client

	// Callbacks
	OnTasksClaimed func(tasks []Task)

	// State
	User     UserInfo
	Features FeatureFlags
	Info     AgentInfo
}

// NewDaemon creates a daemon with the given config and agent client.
func NewDaemon(cfg DaemonConfig, client *Client) *Daemon {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 30 * time.Second
	}
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = 30 * time.Second
	}

	return &Daemon{
		cfg:    cfg,
		client: client,
	}
}

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

	resp, err := d.client.Register(ctx, req)
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}

	d.User = resp.User
	d.Features = resp.Features
	d.Info = AgentInfo{
		ID:        d.cfg.AgentID,
		Name:      d.cfg.AgentName,
		User:      resp.User,
		Features:  resp.Features,
		StartedAt: time.Now(),
	}

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

	heartbeatTicker := time.NewTicker(d.cfg.HeartbeatInterval)
	defer heartbeatTicker.Stop()

	pollTicker := time.NewTicker(d.cfg.PollInterval)
	defer pollTicker.Stop()

	// Initial poll immediately
	d.poll(ctx)

	for {
		select {
		case <-ctx.Done():
			log.Println("Daemon shutting down...")
			return nil

		case <-heartbeatTicker.C:
			d.heartbeat(ctx)

		case <-pollTicker.C:
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

	if err := d.client.Heartbeat(ctx, req); err != nil {
		log.Printf("Heartbeat failed: %v", err)
	}
}

func (d *Daemon) poll(ctx context.Context) {
	tasks, err := d.client.ClaimTasks(ctx, d.cfg.AgentID)
	if err != nil {
		log.Printf("Poll failed: %v", err)
		return
	}

	d.Info.LastPollAt = time.Now()

	if len(tasks) == 0 {
		return
	}

	log.Printf("Claimed %d task(s)", len(tasks))

	if d.OnTasksClaimed != nil {
		d.OnTasksClaimed(tasks)
	}
}

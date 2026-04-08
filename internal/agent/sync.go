package agent

import (
	"context"
	"log"
	"runtime"
	"sync/atomic"
	"time"
)

const (
	// SyncIntervalWatching is the sync interval when someone is viewing the web UI.
	SyncIntervalWatching = 3 * time.Second
	// SyncIntervalIdle is the sync interval when nobody is watching.
	SyncIntervalIdle = 60 * time.Second
)

// SyncClient handles bidirectional state synchronization between the CLI and server.
// It sends the CLI's full execution state and receives all pending server actions
// in a single HTTP round-trip, at an adaptive interval.
type SyncClient struct {
	client *Client
	cfg    DaemonConfig
	state  *LocalState

	// Callbacks — set by the daemon before calling Run.
	OnNewTasks       func(tasks []Task)
	OnControl        func(action, taskID string, deleteFiles bool)
	OnStreamRequest  func(req StreamRequest)
	OnUpgrade        func(version string)
	OnScan           func()
	OnWatchingChange func(watching bool)
	OnSyncSuccess    func() // called after each successful sync (e.g. to update state file)
	GetFreeSlots     func() int
	GetTaskStates    func() []TaskState // returns current state of all active + recently finished tasks

	// SyncNow triggers an immediate sync (e.g., on task completion).
	SyncNow chan struct{}

	watching atomic.Bool
	interval atomic.Int64 // stored as nanoseconds
}

// NewSyncClient creates a sync client.
func NewSyncClient(client *Client, cfg DaemonConfig, state *LocalState) *SyncClient {
	sc := &SyncClient{
		client:  client,
		cfg:     cfg,
		state:   state,
		SyncNow: make(chan struct{}, 1),
	}
	sc.interval.Store(int64(SyncIntervalIdle))
	return sc
}

// Watching returns whether someone is viewing the web UI.
func (sc *SyncClient) Watching() bool {
	return sc.watching.Load()
}

// TriggerSync requests an immediate sync cycle.
func (sc *SyncClient) TriggerSync() {
	select {
	case sc.SyncNow <- struct{}{}:
	default:
	}
}

// Run starts the adaptive sync loop. Blocks until ctx is cancelled.
func (sc *SyncClient) Run(ctx context.Context) error {
	// Initial sync immediately
	sc.doSync(ctx)

	ticker := time.NewTicker(sc.currentInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Final sync to report latest state
			finalCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			sc.doSync(finalCtx)
			return nil

		case <-ticker.C:
			sc.doSync(ctx)
			ticker.Reset(sc.currentInterval())

		case <-sc.SyncNow:
			sc.doSync(ctx)
			ticker.Reset(sc.currentInterval())
		}
	}
}

func (sc *SyncClient) currentInterval() time.Duration {
	return time.Duration(sc.interval.Load())
}

func (sc *SyncClient) doSync(ctx context.Context) {
	req := sc.buildRequest()
	resp, err := sc.client.Sync(ctx, req)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("sync failed: %v", err)
		}
		return
	}
	sc.processResponse(resp)
	sc.adjustInterval(resp.Watching)
	if sc.OnSyncSuccess != nil {
		sc.OnSyncSuccess()
	}
}

func (sc *SyncClient) buildRequest() SyncRequest {
	req := SyncRequest{
		AgentID:     sc.cfg.AgentID,
		Name:        sc.cfg.AgentName,
		Version:     sc.cfg.Version,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		DownloadDir: sc.cfg.DownloadDir,
		StreamPort:  sc.cfg.StreamPort,
		LanIP:       sc.cfg.LanIP,
		TailscaleIP: sc.cfg.TailscaleIP,
	}
	if sc.GetTaskStates != nil {
		req.Tasks = sc.GetTaskStates()
	} else {
		req.Tasks = sc.state.Snapshot()
	}
	if free, total, err := DiskInfo(sc.cfg.DownloadDir); err == nil {
		req.DiskFreeBytes = free
		req.DiskTotalBytes = total
	}
	if sc.GetFreeSlots != nil {
		req.FreeSlots = sc.GetFreeSlots()
	}
	return req
}

func (sc *SyncClient) processResponse(resp *SyncResponse) {
	// New tasks
	if len(resp.NewTasks) > 0 && sc.OnNewTasks != nil {
		log.Printf("sync: received %d new task(s)", len(resp.NewTasks))
		sc.OnNewTasks(resp.NewTasks)
	}

	// Control signals
	for _, ctrl := range resp.Controls {
		log.Printf("sync: control %s on task %s", ctrl.Action, ShortID(ctrl.TaskID))
		if sc.OnControl != nil {
			sc.OnControl(ctrl.Action, ctrl.TaskID, ctrl.DeleteFiles)
		}
	}

	// Stream requests
	for _, sr := range resp.StreamRequests {
		if sc.OnStreamRequest != nil {
			sc.OnStreamRequest(sr)
		}
	}

	// Upgrade
	if resp.Upgrade != nil && resp.Upgrade.Version != "" && sc.OnUpgrade != nil {
		sc.OnUpgrade(resp.Upgrade.Version)
	}

	// Scan
	if resp.Scan && sc.OnScan != nil {
		sc.OnScan()
	}
}

func (sc *SyncClient) adjustInterval(watching bool) {
	prev := sc.watching.Load()
	sc.watching.Store(watching)

	var newInterval time.Duration
	if watching {
		newInterval = SyncIntervalWatching
	} else {
		newInterval = SyncIntervalIdle
	}

	if sc.interval.Swap(int64(newInterval)) != int64(newInterval) {
		log.Printf("sync: interval=%s (watching=%v)", newInterval, watching)
	}

	if prev != watching && sc.OnWatchingChange != nil {
		sc.OnWatchingChange(watching)
	}
}

package agent

import (
	"context"
	"encoding/json"
	"log"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// SyncIntervalWatching is the sync interval when someone is viewing the web UI.
	SyncIntervalWatching = 3 * time.Second
	// SyncIntervalIdle is the sync interval when nobody is watching.
	// Keep this short enough to pick up stream requests quickly without hammering the server.
	SyncIntervalIdle = 10 * time.Second

	// --- Downlink (server→agent realtime) tuning ---

	// downlinkLivenessTimeout is the maximum time to wait for ANY SSE frame
	// (heartbeat comment or event) before declaring the stream dead. The server
	// heartbeats every ~15s; ~2.5× gives slack for jitter while still catching a
	// path that connects 200 OK but silently buffers (delivers nothing until
	// close) — the failure mode that justifies the long-poll fallback.
	downlinkLivenessTimeout = 40 * time.Second
	// sseReconnectDelay is the pause between SSE connection attempts.
	sseReconnectDelay = 2 * time.Second
	// maxSSEFailures is the number of consecutive failed/dead SSE attempts
	// before "auto" mode falls back to the long-poll wake downlink.
	maxSSEFailures = 3
	// downlinkFallbackWindow is how long to ride long-poll before re-probing SSE,
	// so a transient proxy hiccup doesn't pin the agent on polling forever.
	downlinkFallbackWindow = 5 * time.Minute
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
	OnStreamSession  func(sess StreamSession)
	OnUpgrade        func(version string)
	OnScan           func()
	OnWatchingChange func(watching bool)
	OnSyncSuccess    func() // called after each successful sync (e.g. to update state file)
	GetFreeSlots     func() int
	GetTaskStates    func() []TaskState // returns current state of all active + recently finished tasks
	// GetVPNState returns the live managed-VPN split-tunnel state (whether the
	// WireGuard tunnel is up, the mode, and the exit server) so the web can track
	// which agent holds the single WG slot.
	GetVPNState func() (active bool, mode, server string)
	// GetFunnelURL returns the CloudFlare Quick Tunnel public hostname if one
	// is active, else "". Sent on every sync so the web picks it up live.
	GetFunnelURL func() string
	// OnDeleteFiles is called when the server requests file deletion from disk.
	// It should delete the files and return the IDs of successfully deleted items.
	OnDeleteFiles func(items []LibraryDeleteRequest) []int

	// SyncNow triggers an immediate sync (e.g., on task completion).
	SyncNow chan struct{}

	watching atomic.Bool
	interval atomic.Int64 // stored as nanoseconds

	// livenessTimeout is the max wait for any SSE frame before the downlink
	// treats the stream as dead/buffered. Defaults to downlinkLivenessTimeout;
	// overridable in tests.
	livenessTimeout time.Duration

	// pendingDeleteConfirmed holds item IDs to report as deleted in the next sync.
	pendingDeleteMu        sync.Mutex
	pendingDeleteConfirmed []int
	// deleteInFlight tracks item IDs currently being processed or awaiting confirmation.
	// Prevents the same file from being passed to OnDeleteFiles multiple times.
	deleteInFlight map[int]struct{}
}

// NewSyncClient creates a sync client.
func NewSyncClient(client *Client, cfg DaemonConfig, state *LocalState) *SyncClient {
	sc := &SyncClient{
		client:          client,
		cfg:             cfg,
		state:           state,
		SyncNow:         make(chan struct{}, 1),
		livenessTimeout: downlinkLivenessTimeout,
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
	// Start the realtime downlink in background — pushes immediate syncs +
	// typed control commands on demand (SSE-first, long-poll fallback).
	go sc.runDownlink(ctx)

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
		CanDelete:   sc.cfg.CanDelete,
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
	if sc.GetVPNState != nil {
		req.VPNActive, req.VPNMode, req.VPNServer = sc.GetVPNState()
	}
	if sc.GetFunnelURL != nil {
		req.FunnelURL = sc.GetFunnelURL()
	}
	// Flush confirmed deletions from previous cycle.
	// Once flushed, remove IDs from deleteInFlight — the server will stop sending
	// them after this sync, so deduplication protection is no longer needed.
	sc.pendingDeleteMu.Lock()
	if len(sc.pendingDeleteConfirmed) > 0 {
		req.DeleteConfirmed = sc.pendingDeleteConfirmed
		for _, id := range sc.pendingDeleteConfirmed {
			delete(sc.deleteInFlight, id)
		}
		sc.pendingDeleteConfirmed = nil
	}
	sc.pendingDeleteMu.Unlock()
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

	// HLS streaming sessions.
	for _, ws := range resp.StreamSessions {
		if sc.OnStreamSession != nil {
			sc.OnStreamSession(ws)
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

	// File deletions requested by the server — deduplicate against in-flight items
	if len(resp.FilesToDelete) > 0 && sc.OnDeleteFiles != nil {
		sc.pendingDeleteMu.Lock()
		if sc.deleteInFlight == nil {
			sc.deleteInFlight = make(map[int]struct{})
		}
		var newItems []LibraryDeleteRequest
		for _, item := range resp.FilesToDelete {
			if _, inFlight := sc.deleteInFlight[item.ItemID]; !inFlight {
				newItems = append(newItems, item)
				sc.deleteInFlight[item.ItemID] = struct{}{}
			}
		}
		sc.pendingDeleteMu.Unlock()

		if len(newItems) > 0 {
			// Run deletions off the sync goroutine — disk I/O must not block the
			// next sync tick. Confirmations are picked up on the next regular cycle.
			go func(items []LibraryDeleteRequest) {
				confirmed := sc.OnDeleteFiles(items)
				if len(confirmed) > 0 {
					sc.pendingDeleteMu.Lock()
					sc.pendingDeleteConfirmed = append(sc.pendingDeleteConfirmed, confirmed...)
					sc.pendingDeleteMu.Unlock()
				}
			}(newItems)
		}
	}
}

// runWakeListener holds a long-poll connection to /api/internal/agent/wake.
// When the server resolves it with wake=true (e.g., a stream was requested),
// it triggers an immediate sync so the CLI acts in <100ms instead of waiting
// for the next scheduled interval. Reconnects immediately after each response
// so coverage is continuous. Runs until ctx is cancelled.
func (sc *SyncClient) runWakeListener(ctx context.Context) {
	const retryDelay = 2 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		woke, err := sc.client.WaitForWake(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("wake listener: %v (retrying in %s)", err, retryDelay)
			select {
			case <-ctx.Done():
				return
			case <-time.After(retryDelay):
			}
			continue
		}
		if woke {
			log.Printf("wake signal received — syncing immediately")
			sc.TriggerSync()
		}
		// On timeout (woke=false) or after a wake, reconnect immediately.
	}
}

// runWakeListenerFor runs the long-poll wake listener for up to `dur`, then
// returns so the caller can re-probe SSE. Used as the auto-mode fallback.
func (sc *SyncClient) runWakeListenerFor(ctx context.Context, dur time.Duration) {
	childCtx, cancel := context.WithTimeout(ctx, dur)
	defer cancel()
	sc.runWakeListener(childCtx)
}

// downlinkMode resolves the configured downlink transport:
//   - "auto" (default): SSE-first, fall back to long-poll wake if SSE is
//     unavailable or silently buffered, then periodically re-probe SSE.
//   - "sse": SSE only, no long-poll fallback (testing / known-good networks).
//   - "poll": long-poll wake only (the pre-0.14 behavior).
func (sc *SyncClient) downlinkMode() string {
	switch strings.ToLower(strings.TrimSpace(sc.cfg.Downlink)) {
	case "poll":
		return "poll"
	case "sse":
		return "sse"
	default:
		return "auto"
	}
}

// runDownlink is the server→agent realtime loop. It supersedes the bare
// long-poll wake listener: an SSE connection pushes typed control commands and
// sync nudges over a single persistent connection, with the long-poll wake as a
// buffering-tolerant fallback (long-poll survives proxies that buffer the
// response body and break SSE). Runs until ctx is cancelled.
func (sc *SyncClient) runDownlink(ctx context.Context) {
	switch sc.downlinkMode() {
	case "poll":
		log.Printf("downlink: long-poll wake (downlink=poll)")
		sc.runWakeListener(ctx)
	case "sse":
		log.Printf("downlink: SSE only (downlink=sse) — no long-poll fallback")
		sc.runSSELoop(ctx, false)
	default:
		sc.runSSELoop(ctx, true)
	}
}

// runSSELoop maintains the SSE downlink, reconnecting across server recycles
// and transient drops. When allowFallback is true (auto mode), it switches to
// the long-poll wake after maxSSEFailures consecutive dead attempts, then
// re-probes SSE after downlinkFallbackWindow.
func (sc *SyncClient) runSSELoop(ctx context.Context, allowFallback bool) {
	failures := 0
	for ctx.Err() == nil {
		healthy := sc.runEventStreamOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if healthy {
			failures = 0
			// A healthy stream that ended is a normal server recycle — reconnect.
			sc.sleep(ctx, sseReconnectDelay)
			continue
		}

		failures++
		if allowFallback && failures >= maxSSEFailures {
			log.Printf("downlink: SSE unavailable after %d attempts — falling back to long-poll for %s", failures, downlinkFallbackWindow)
			sc.runWakeListenerFor(ctx, downlinkFallbackWindow)
			failures = 0
			continue
		}
		sc.sleep(ctx, sseReconnectDelay)
	}
}

// runEventStreamOnce opens one SSE connection and consumes it until it dies or
// ctx is cancelled. Returns true if the stream was "healthy" — i.e. it
// delivered at least one frame (event or heartbeat) — and false if it failed to
// connect or delivered nothing within downlinkLivenessTimeout (dead or silently
// buffered). The caller uses that signal to decide whether to fall back.
func (sc *SyncClient) runEventStreamOnce(ctx context.Context) bool {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := sc.client.OpenEventStream(streamCtx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("downlink: SSE connect failed: %v", err)
		}
		return false
	}
	defer stream.Close()

	healthy := false
	liveness := time.NewTimer(sc.livenessTimeout)
	defer liveness.Stop()

	for {
		select {
		case <-ctx.Done():
			return healthy
		case <-liveness.C:
			// No frame within the deadline. The server heartbeats every ~15s, so
			// silence past livenessTimeout (40s) means the path is dead OR
			// silently buffering — INCLUDING a proxy that flushed the connect
			// preamble (one ping) then stalled. Return false REGARDLESS of any
			// earlier frame, so this counts toward the long-poll fallback; a
			// stream that flushes one ping and goes quiet must not be treated as
			// healthy or the fallback never triggers for partial bufferers.
			if ctx.Err() == nil {
				log.Printf("downlink: no SSE frame within %s — dropping (dead or buffered path)", sc.livenessTimeout)
			}
			return false
		case ev, ok := <-stream.Events():
			if !ok {
				if e := stream.Err(); e != nil && ctx.Err() == nil {
					log.Printf("downlink: SSE stream ended: %v", e)
				}
				return healthy
			}
			if !healthy {
				// First frame on this connection — the path flushes, so log once
				// (on a silently-buffered path no frame ever arrives and we never
				// claim connected).
				log.Printf("downlink: SSE connected")
			}
			healthy = true
			if !liveness.Stop() {
				select {
				case <-liveness.C:
				default:
				}
			}
			liveness.Reset(sc.livenessTimeout)
			sc.handleDownlinkEvent(ev)
		}
	}
}

// handleDownlinkEvent applies one pushed downlink event. Pings are liveness-only;
// "sync" nudges an immediate full sync; "command" carries typed control actions
// applied via the same OnControl callback /agent/sync uses (idempotent, so the
// authoritative sync re-delivering them is harmless).
func (sc *SyncClient) handleDownlinkEvent(ev DownlinkEvent) {
	switch ev.Event {
	case DownlinkEventPing:
		// Liveness only.
	case DownlinkEventSync:
		sc.TriggerSync()
	case DownlinkEventCommand:
		var cmd CommandEvent
		if err := json.Unmarshal(ev.Data, &cmd); err != nil {
			log.Printf("downlink: bad command payload: %v", err)
			return
		}
		for _, ctrl := range cmd.Controls {
			log.Printf("downlink: control %s on task %s", ctrl.Action, ShortID(ctrl.TaskID))
			if sc.OnControl != nil {
				sc.OnControl(ctrl.Action, ctrl.TaskID, ctrl.DeleteFiles)
			}
		}
	default:
		// Unknown event from a newer server — ignore forward-compatibly.
	}
}

// sleep blocks for d or until ctx is cancelled.
func (sc *SyncClient) sleep(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
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

	// Trigger an immediate sync when entering watching mode so stream requests
	// are picked up right away without waiting for the next scheduled interval.
	if watching && !prev {
		sc.TriggerSync()
	}

	if prev != watching && sc.OnWatchingChange != nil {
		sc.OnWatchingChange(watching)
	}
}

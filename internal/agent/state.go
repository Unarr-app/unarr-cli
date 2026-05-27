package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/torrentclaw/unarr/internal/config"
)

// ErrDaemonNotRunning is returned when no daemon state file exists on disk.
// Callers may wrap it with %w; downstream code uses errors.Is to detect it.
// NOTE: the message text is matched by the sentry package (string-match, to
// avoid an import cycle). Keep the prefix "daemon does not appear to be
// running" stable, or update sentry.daemonNotRunningMarker accordingly.
var ErrDaemonNotRunning = errors.New("daemon does not appear to be running (state file not found)")

// DaemonState is written to disk every heartbeat for external tools to read.
type DaemonState struct {
	AgentID         string         `json:"agentId"`
	Status          string         `json:"status"` // running | upgrading | shutting_down
	Version         string         `json:"version"`
	PID             int            `json:"pid"`
	StartedAt       time.Time      `json:"startedAt"`
	LastHeartbeat   time.Time      `json:"lastHeartbeat"`
	ActiveTasks     int            `json:"activeTasks"`
	CompletedCount  int            `json:"completedCount"`
	FailedCount     int            `json:"failedCount"`
	TotalDownloaded int64          `json:"totalDownloaded"`
	MethodStats     map[string]int `json:"methodStats,omitempty"`

	// Managed-VPN split-tunnel state, so `unarr vpn status` can report whether
	// torrent traffic is actually being routed through the tunnel (vs. the daemon
	// running but the tunnel having failed to come up → downloading in the clear).
	VPNActive bool   `json:"vpnActive,omitempty"`
	VPNMode   string `json:"vpnMode,omitempty"`   // managed | self-hosted
	VPNServer string `json:"vpnServer,omitempty"` // WireGuard endpoint (ip:port)

	// CloudFlare Quick Tunnel state, so `unarr funnel status` can report the
	// HTTPS hostname the daemon is reachable at from anywhere on the internet.
	// Empty when the funnel is off or hasn't registered yet.
	FunnelURL string `json:"funnelUrl,omitempty"`
}

// stateFilePathFn is overridable for testing.
var stateFilePathFn = func() string {
	return filepath.Join(config.DataDir(), "daemon.state.json")
}

// StateFilePath returns the path to the daemon state file.
func StateFilePath() string {
	return stateFilePathFn()
}

// WriteState writes the daemon state to disk (best-effort, never errors).
func WriteState(state *DaemonState) {
	path := StateFilePath()
	dir := filepath.Dir(path)
	os.MkdirAll(dir, 0o755)

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}

	// Write to temp file then rename for atomicity. 0o600 keeps the file
	// readable only by the owning user — the state contains agentID, PID
	// and counters which are useful to a co-tenant on a shared host for
	// fingerprinting the daemon, and we already use 0o600 for the config
	// file. No need for cross-user readability here.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	os.Rename(tmp, path)
}

// ReadState reads the daemon state from disk. Returns nil if not found or
// unreadable. Use LoadState when callers need to distinguish "not running"
// from "state file corrupted".
func ReadState() *DaemonState {
	state, _ := LoadState()
	return state
}

// LoadState reads the daemon state and returns explicit errors:
//   - ErrDaemonNotRunning when the state file does not exist
//   - a wrapped json error when the file exists but cannot be decoded
//     (a real bug worth reporting to Sentry)
func LoadState() (*DaemonState, error) {
	data, err := os.ReadFile(StateFilePath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrDaemonNotRunning
		}
		return nil, err
	}
	var state DaemonState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode daemon state %s: %w", StateFilePath(), err)
	}
	return &state, nil
}

// RemoveState deletes the state file (called on clean shutdown).
func RemoveState() {
	os.Remove(StateFilePath())
}

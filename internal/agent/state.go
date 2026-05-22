package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/torrentclaw/unarr/internal/config"
)

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

// ReadState reads the daemon state from disk. Returns nil if not found.
func ReadState() *DaemonState {
	data, err := os.ReadFile(StateFilePath())
	if err != nil {
		return nil
	}
	var state DaemonState
	if json.Unmarshal(data, &state) != nil {
		return nil
	}
	return &state
}

// RemoveState deletes the state file (called on clean shutdown).
func RemoveState() {
	os.Remove(StateFilePath())
}

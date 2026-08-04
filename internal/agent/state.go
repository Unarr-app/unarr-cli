package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/config"
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

	// LogFile is the log file this daemon OWNS and rotates from the inside
	// (`unarr start --log-file …`). Empty when the supervisor still owns the
	// redirect, or when the daemon runs in the foreground. It is how an external
	// rotator answers "is a live process writing this file?" — see ClaimLogFile.
	// Never set it by hand: WriteState stamps it from this process's own claim.
	LogFile string `json:"logFile,omitempty"`

	// Managed-VPN split-tunnel state, so `unarr vpn status` can report whether
	// torrent traffic is actually being routed through the tunnel (vs. the daemon
	// running but the tunnel having failed to come up → downloading in the clear).
	VPNActive bool   `json:"vpnActive,omitempty"`
	VPNMode   string `json:"vpnMode,omitempty"`   // managed | self-hosted
	VPNServer string `json:"vpnServer,omitempty"` // WireGuard endpoint (ip:port)
	// VPNRequired mirrors config [downloads.vpn] required — the fail-closed P2P
	// kill-switch. VPNBlocking is true when the kill-switch is on but no healthy
	// tunnel is up, so torrent is currently DISABLED (safe, not a leak). Read by
	// `unarr vpn status` and `unarr doctor` to show whether P2P is blocked.
	VPNRequired bool `json:"vpnRequired,omitempty"`
	VPNBlocking bool `json:"vpnBlocking,omitempty"`

	// CloudFlare Quick Tunnel state, so `unarr funnel status` can report the
	// HTTPS hostname the daemon is reachable at from anywhere on the internet.
	// Empty when the funnel is off or hasn't registered yet.
	FunnelURL string `json:"funnelUrl,omitempty"`

	// Local control plane (see internal/control): the loopback port `unarr
	// downloads` and the desktop tray drive downloads through, plus the token
	// they must present. Published here because the state file is already the
	// daemon's handshake with local tooling and is 0o600 — the token is only
	// readable by the user who owns the daemon.
	//
	// Zero/empty means "no control plane": either an older daemon, or one whose
	// control listener failed to bind. Clients must treat that as ErrNoDaemon
	// and fall back to the offline path rather than assuming a port.
	ControlPort  int    `json:"controlPort,omitempty"`
	ControlToken string `json:"controlToken,omitempty"`
}

// stateFilePathFn is overridable for testing.
var stateFilePathFn = func() string {
	return filepath.Join(config.DataDir(), "daemon.state.json")
}

// StateFilePath returns the path to the daemon state file.
func StateFilePath() string {
	return stateFilePathFn()
}

// stateSealed latches at shutdown, after which WriteState is a no-op.
//
// Removing the state file is not enough to make a clean stop look clean: the
// shutdown path cancels the daemon context and deregisters, but the sync loop
// and the task reporters are still unwinding, and any one of them calling
// WriteState on its way out RE-CREATES the file from the in-memory snapshot —
// complete with "status": "running" and the last heartbeat from before the
// stop. The resurrected file then outlives the process, and unarr-desktop reads
// "running + PID gone" as a crash and mails a report for a stop the user asked
// for. (Observed directly: a SIGTERM shutdown that logged "Agent deregistered"
// and "Daemon stopped." still left a state file behind.)
//
// So the removal is paired with a latch. Once sealed, no straggler can undo it.
//
// The latch and the write share a mutex rather than being a lone atomic flag,
// and that is not belt-and-braces: a writer that had ALREADY passed a flag check
// still has a MkdirAll, a WriteFile and a Rename ahead of it, so its rename can
// land after the seal and after the final removal. Checking a flag only narrows
// the window; holding the lock across the whole write closes it. SealState then
// waits for any write in flight, and every later one returns before touching the
// disk — so the reap that follows is final.
var (
	stateWriteMu sync.Mutex
	stateSealed  atomic.Bool
)

// SealState stops this process from ever writing the state file again. It blocks
// until any write already in progress has finished, so the caller can remove the
// file afterwards and know nothing will put it back. Called once, at the very
// end of the daemon's life — see ReapOwnState and cmd.runDaemon.
func SealState() {
	stateWriteMu.Lock()
	defer stateWriteMu.Unlock()
	stateSealed.Store(true)
}

// resetStateSeal lifts the latch. Test-only: the seal is deliberately one-way
// for the life of a real process.
func resetStateSeal() {
	stateWriteMu.Lock()
	defer stateWriteMu.Unlock()
	stateSealed.Store(false)
}

// WriteState writes the daemon state to disk (best-effort, never errors).
func WriteState(state *DaemonState) {
	stateWriteMu.Lock()
	defer stateWriteMu.Unlock()
	if stateSealed.Load() {
		return // shutting down: see stateSealed
	}
	// Ownership of the log file is a property of THIS process, not of whatever
	// snapshot a caller happens to hold, so it is stamped on every write rather
	// than being carried around in DaemonState. A full-struct overwrite (Register
	// rebuilds d.State from scratch) can therefore never drop it, and a release
	// can never be undone by a straggler write.
	state.LogFile = OwnedLogFile()
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

// ReapOwnState removes the state file, but ONLY while it still names this
// process. It is how a deliberate exit stops looking like a crash: the tray
// reads "state file says running + PID gone" as a death worth mailing a report
// about, and every non-signal exit path used to leave exactly that behind — a
// fatal startup error, or the credential-revoked shutdown the daemon itself
// documents as "clean, expected … not a crash".
//
// The PID guard is load-bearing, not defensive: a dev agent and the production
// agent share one data dir on purpose (only the lock is config-scoped), so an
// unconditional remove would let either one delete the other's live state.
//
// Deliberately NOT deferred by its callers — a panic must unwind past it and
// leave the state file in place, because that IS the crash the report is for.
func ReapOwnState() {
	if st := ReadState(); st != nil && st.PID == os.Getpid() {
		RemoveState()
	}
}

// RemoveState deletes the state file (called on clean shutdown).
func RemoveState() {
	os.Remove(StateFilePath())
}

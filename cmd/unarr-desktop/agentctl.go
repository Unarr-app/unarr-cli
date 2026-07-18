package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
)

// unarrBin resolves the headless `unarr` daemon binary: PATH first, then a
// sibling of this executable (installers drop `unarr` + `unarr-desktop`
// together), then a bare name as a last resort.
func unarrBin() string {
	if p, err := exec.LookPath("unarr"); err == nil {
		return p
	}
	if self, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(self), "unarr")
		if runtime.GOOS == "windows" {
			cand += ".exe"
		}
		if _, statErr := os.Stat(cand); statErr == nil {
			return cand
		}
	}
	return "unarr"
}

// runUnarr execs `unarr <args…>` DETACHED — the daemon's lifetime must not be
// tied to the tray process. Returns only the spawn error, not the exit status
// (stop/restart hand off to the daemon's own service/PID-aware control logic).
func runUnarr(args ...string) error {
	return exec.Command(unarrBin(), args...).Start()
}

// runUnarrOutput execs `unarr <args…>` and returns its combined output —
// for short, synchronous queries (version, log dump), never for control.
func runUnarrOutput(args ...string) ([]byte, error) {
	return exec.Command(unarrBin(), args...).CombinedOutput()
}

// agentStatus is the slice of daemon state the tray surfaces — read from the
// same on-disk state file `unarr status` uses, so the tray never drifts from it.
type agentStatus struct {
	running bool
	// crashed: the state file is still on disk claiming "running" but the PID
	// is gone. A clean shutdown removes the state file, and upgrade/shutdown
	// transitions write status "upgrading"/"shutting_down" first — so this
	// combination only happens when the daemon died without cleaning up.
	crashed bool
	pid     int
	version string
	agentID string
	tasks   int
}

func readStatus() agentStatus {
	st := agent.ReadState()
	if st == nil || st.PID == 0 {
		return agentStatus{}
	}
	if !agent.IsProcessAlive(st.PID) {
		return agentStatus{
			crashed: st.Status == "running",
			pid:     st.PID,
			version: st.Version,
			agentID: st.AgentID,
		}
	}
	return agentStatus{
		running: true,
		pid:     st.PID,
		version: st.Version,
		agentID: st.AgentID,
		tasks:   st.ActiveTasks,
	}
}

// configPath is the active config.toml (honors UNARR_CONFIG_DIR / --config the
// same way the daemon does).
func configPath() string { return config.FilePath() }

// pausedMarkerPath marks a tray-initiated pause. Pause and stop are the same
// daemon operation (clean stop); the on-disk marker is what lets the tray show
// "paused" (amber) instead of "stopped" (gray) across tray restarts. Lives next
// to the daemon state file so both surfaces share one data dir.
func pausedMarkerPath() string {
	return filepath.Join(filepath.Dir(agent.StateFilePath()), "desktop.paused")
}

// markPaused records/clears the tray-initiated pause. Best-effort — worst case
// the icon shows "stopped" instead of "paused".
func markPaused(on bool) {
	if on {
		if err := os.WriteFile(pausedMarkerPath(), []byte("paused by unarr-desktop\n"), 0o644); err != nil {
			// non-fatal: state degrades to "stopped"
			return
		}
		return
	}
	_ = os.Remove(pausedMarkerPath())
}

func isPausedMarker() bool {
	_, err := os.Stat(pausedMarkerPath())
	return err == nil
}

// dumpLogs writes the daemon's combined log output to a temp file so it can be
// opened in a viewer without a terminal.
func dumpLogs() (string, error) {
	out := collectLogs()
	f, ferr := os.CreateTemp("", "unarr-logs-*.txt")
	if ferr != nil {
		return "", ferr
	}
	defer f.Close()
	if _, werr := f.Write(out); werr != nil {
		return "", werr
	}
	return f.Name(), nil
}

// collectLogs delegates to `unarr daemon logs` — the daemon knows where its
// logs live (journald for a systemd service, a file otherwise), so the tray
// never has to guess a path. Returns an actionable placeholder when nothing is
// available; any error the command printed is part of the text, so the user
// always sees something useful. Shared by "View logs" (temp file) and the
// support-report path (request body).
func collectLogs() []byte {
	out, err := runUnarrOutput("daemon", "logs")
	if len(out) == 0 {
		msg := "No logs available."
		if err != nil {
			msg += " (" + err.Error() + ")"
		}
		out = []byte(msg + "\nIf the agent runs in the foreground, logs go to its" +
			" terminal. Install it as a service (unarr daemon install) to persist them.\n")
	}
	return out
}

// openPath opens a file or directory with the OS default handler (no terminal).
func openPath(path string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", path).Start()
	case "windows":
		return exec.Command("cmd", "/c", "start", "", path).Start()
	default:
		return exec.Command("xdg-open", path).Start()
	}
}

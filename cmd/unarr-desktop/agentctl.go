package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/torrentclaw/unarr/internal/agent"
	"github.com/torrentclaw/unarr/internal/config"
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

// agentStatus is the slice of daemon state the tray surfaces — read from the
// same on-disk state file `unarr status` uses, so the tray never drifts from it.
type agentStatus struct {
	running bool
	pid     int
	version string
	tasks   int
}

func readStatus() agentStatus {
	st := agent.ReadState()
	if st == nil || st.PID == 0 || !agent.IsProcessAlive(st.PID) {
		return agentStatus{}
	}
	return agentStatus{running: true, pid: st.PID, version: st.Version, tasks: st.ActiveTasks}
}

// configPath is the active config.toml (honors UNARR_CONFIG_DIR / --config the
// same way the daemon does).
func configPath() string { return config.FilePath() }

// dumpLogs delegates to `unarr daemon logs` — the daemon knows where its logs
// live (journald for a systemd service, a file otherwise), so the tray never has
// to guess a path. The combined output is written to a temp file so it can be
// opened in a viewer without a terminal; any error the command printed is part
// of that text, so the user always sees something actionable.
func dumpLogs() (string, error) {
	out, err := exec.Command(unarrBin(), "daemon", "logs").CombinedOutput()
	if len(out) == 0 {
		msg := "No logs available."
		if err != nil {
			msg += " (" + err.Error() + ")"
		}
		out = []byte(msg + "\nIf the agent runs in the foreground, logs go to its" +
			" terminal. Install it as a service (unarr daemon install) to persist them.\n")
	}
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

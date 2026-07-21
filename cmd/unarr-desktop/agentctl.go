package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/service"
)

// resolveUnarrBin locates the headless `unarr` daemon binary: PATH first, then a
// sibling of this executable (installers drop `unarr` + `unarr-desktop`
// together). The bool reports whether it was actually found — false means only
// the bare-name fallback is returned, i.e. no CLI is installed (player-only).
func resolveUnarrBin() (string, bool) {
	if p, err := exec.LookPath("unarr"); err == nil {
		return p, true
	}
	if self, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(self), "unarr")
		if runtime.GOOS == "windows" {
			cand += ".exe"
		}
		if _, statErr := os.Stat(cand); statErr == nil {
			return cand, true
		}
	}
	return "unarr", false
}

// unarrBin is the path to exec the daemon with (bare "unarr" if not found).
func unarrBin() string { p, _ := resolveUnarrBin(); return p }

// hasCLI reports whether the `unarr` daemon binary is installed. When it isn't,
// the tray runs in player-only mode: no daemon to control, so the pause/resume/
// restart + account rows are hidden and an "Enable downloads & library" CTA is
// shown instead. Re-checked every tick, so installing the CLI later promotes the
// menu without a restart.
func hasCLI() bool { _, ok := resolveUnarrBin(); return ok }

// runUnarrOutput execs `unarr <args…>` and returns its combined output —
// for short, synchronous queries (version, log dump), never for control.
// Bounded: a binary on a hung network mount (or blocked by AV) must not hang
// the calling goroutine forever — the account loop would never run again.
func runUnarrOutput(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, unarrBin(), args...).CombinedOutput()
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
	// vpnBlocking: the fail-closed VPN kill-switch is on and no healthy tunnel
	// is up, so torrent downloads are DISABLED. Safe, deliberate — and a total
	// functional outage that the tray used to render as a healthy green agent.
	vpnBlocking bool
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
		running:     true,
		pid:         st.PID,
		version:     st.Version,
		agentID:     st.AgentID,
		tasks:       st.ActiveTasks,
		vpnBlocking: st.VPNBlocking,
	}
}

// configPath is the active config.toml (honors UNARR_CONFIG_DIR / --config the
// same way the daemon does).
func configPath() string { return config.FilePath() }

// openDownloadsFolder opens the agent's configured download directory in the OS
// file manager. It reads the same config the daemon uses (honoring
// UNARR_DOWNLOAD_DIR); a load error or an unset/missing dir degrades to a
// stderr line (openFile stats the path), never a crash — the tray runs where a
// CLI is installed, but a half-configured install must not take the menu down.
func openDownloadsFolder() {
	cfg, err := config.Load(config.FilePath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "unarr-desktop: downloads dir:", err)
		return
	}
	cfg.ApplyEnvOverrides()
	openFile(cfg.Download.Dir)
}

// reapStaleState removes the state file a dead daemon left behind, but only
// when it still names the given PID (never races a freshly started daemon).
// Used for tray-initiated stops: a 1.5.1-and-older `unarr stop` can leave the
// orphan (early-stop race on unix; taskkill /f always on Windows), which would
// otherwise read as a crash 20s later and email a false report. Newer CLIs
// reap it themselves — this makes the tray safe with ANY installed CLI.
func reapStaleState(pid int) {
	st := agent.ReadState()
	if st != nil && st.PID == pid && !agent.IsProcessAlive(pid) {
		agent.RemoveState()
	}
}

// daemonCtl maps a tray control ("stop"/"start") to the argv that actually
// achieves it. Under a respawning supervisor both must go through the service
// manager: `unarr stop` on an old CLI only kills a PID that systemd revives 10s
// later (Restart=always) — the "I pause it and it turns itself back on" bug —
// and `unarr start` would spawn a daemon outside the unit that dies with the
// tray and fights the one systemd owns.
func daemonCtl(action string) []string {
	if service.Respawns() {
		return []string{"daemon", action}
	}
	return []string{action}
}

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

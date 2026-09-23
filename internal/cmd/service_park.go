package cmd

// A service with no credential is parked, not restart-looped.
//
// The daemon cannot run without an API key, and no amount of restarting gives
// it one. Yet systemd (Restart=always) and launchd (KeepAlive) brought it back
// every ten seconds regardless. A dashboard delete wipes the credential; the
// daemon itself stays up waiting for a sign-in, but the first restart after
// that (a crash, a reboot, a self-update) comes back with nothing to sign in
// with — a PC whose agent was deleted on 2026-09-22 then logged over 7,000
// starts in a day, each exiting on "no API key configured". The service is
// ours — `unarr daemon install` put it there — so it is also ours to stand
// down, and to bring back once the user signs in:
//
//   - the daemon, finding itself the installed service with no API key, leaves
//     a marker (service.parked), asks its supervisor to stop it (parkService)
//     and exits exitNeedsSignIn. The unit stays enabled: after a reboot it
//     starts once, finds no key and parks again — one line per boot, not a loop;
//   - `unarr login` starts the service again, but only one that parked itself:
//     a service the user stopped on purpose stays stopped. `unarr init`
//     reinstalls it anyway, and a daemon that IS running picks the new key up
//     on its own (d.ReloadCredential).
//
// Only a missing key parks. A config that failed to load, or a key without an
// agent ID, keep the old restart: those are recoverable without a sign-in, and
// parking would wait for one that never comes.
//
// Docker is left alone on purpose: the container's restart policy is the
// operator's, and `up` has its own guard against re-minting a deleted agent
// (revoked_marker.go).

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/service"
	"github.com/fatih/color"
)

// exitNeedsSignIn is EX_CONFIG from sysexits.h. The systemd unit counts it as
// a success that must not restart (SuccessExitStatus + RestartPreventExitStatus),
// so a new install stays down — and reads "inactive", not "failed" — even when
// the stop request below cannot reach the user manager.
const exitNeedsSignIn = 78

// needsSignInError is a daemon start that cannot proceed until someone signs in.
// It is an expected state, not a failure: Execute exits exitNeedsSignIn without
// the red "Error:" line and without a Sentry event.
type needsSignInError struct{ msg string }

func (e *needsSignInError) Error() string { return e.msg }

func needsSignIn(format string, args ...any) error {
	return &needsSignInError{msg: fmt.Sprintf(format, args...)}
}

func isNeedsSignIn(err error) bool {
	var e *needsSignInError
	return errors.As(err, &e)
}

// parkedMarkerPath sits with the config.toml in use, like revoked.json: it
// belongs to the config the parked service runs with, so a dev agent's
// `--config` neither spends nor finds the installed service's marker.
func parkedMarkerPath() string {
	return filepath.Join(filepath.Dir(resolvedConfigPath()), "service.parked")
}

func clearParkedMarker() { os.Remove(parkedMarkerPath()) }

func parkedMarkerExists() bool {
	_, err := os.Stat(parkedMarkerPath())
	return err == nil
}

// underInstalledService reports whether this process was started by the
// service `unarr daemon install` created, as opposed to a foreground or
// detached `unarr start`, or a container.
func underInstalledService() bool {
	switch runtime.GOOS {
	case "linux":
		return runningAsSystemdUnit(readSelfCgroup())
	case "darwin":
		return launchdJobLabel() != ""
	case "windows":
		return launchedByTaskShim()
	}
	return false
}

// procEntry is one row of a process snapshot.
type procEntry struct {
	parent uint32
	exe    string
}

// shimChain reports whether self was started the way the scheduled task's
// launcher shim starts the daemon: wscript.exe runs unarr-launch.vbs, which
// runs `cmd /c unarr start ... >> unarr.boot.log`. The console is not a usable
// signal there — cmd.exe gives the daemon a hidden console, so stdin reads as
// a terminal (measured on Windows 11, 2026-09-23) — but nothing else launches
// the daemon through cmd.exe under wscript.exe.
func shimChain(procs map[uint32]procEntry, self uint32) bool {
	me, ok := procs[self]
	if !ok {
		return false
	}
	parent, ok := procs[me.parent]
	if !ok || !strings.EqualFold(parent.exe, "cmd.exe") {
		return false
	}
	grand, ok := procs[parent.parent]
	return ok && strings.EqualFold(grand.exe, "wscript.exe")
}

// signInHint is what a daemon with no API key tells the user. Under the
// installed service the generic hint is wrong: its non-interactive branch
// points at `unarr up`, which runs the agent in the user's terminal and leaves
// the service down.
func signInHint(apiURL string) string {
	if underInstalledService() {
		return "sign in with `unarr login`; the background service starts again by itself"
	}
	return setupHint(apiURL)
}

// parkService asks the service manager that launched this process not to launch
// it again, and records that it did. A no-op when the daemon was not started by
// the installed service.
//
// Neither request is waited on. Both managers answer a stop by signalling the
// process, and this process is the one being stopped: waiting would only hold
// its exit hostage to its own termination. That SIGTERM is ignored for the same
// reason — it would land within milliseconds and kill the process before it
// prints why it stopped, leaving the journal a bare "status=15/TERM". The
// ignore is inherited by every child started afterwards (Go resets only the
// signals it handles), which here is only the stop request itself; nothing
// else may be spawned once this has run.
func parkService() {
	if !underInstalledService() {
		return
	}
	err := os.MkdirAll(filepath.Dir(parkedMarkerPath()), 0o755)
	if err == nil {
		err = os.WriteFile(parkedMarkerPath(), []byte("parked until sign-in\n"), 0o644)
	}
	if err != nil {
		// Without the marker a sign-in will not bring the service back; the
		// user can still start it, so this is worth a line, not a failure.
		fmt.Fprintf(os.Stderr, "  could not record the parked service at %s: %v\n", parkedMarkerPath(), err)
	}
	switch runtime.GOOS {
	case "linux":
		signal.Ignore(syscall.SIGTERM)
		// --no-block queues the stop job and returns; the job, not a restart,
		// then owns this exit.
		if out, err := svcOutput("systemctl", "--user", "stop", "--no-block", service.SystemdUnitName); err != nil {
			fmt.Fprintf(os.Stderr, "  could not park the service (%s) - it will keep restarting until you sign in\n", firstLine(out, err))
			return
		}
		fmt.Fprintln(os.Stderr, "  Background service stopped until you sign in.")
	case "darwin":
		home, _ := os.UserHomeDir()
		a, err := newLaunchdAgent(home)
		if err == nil {
			// The job that runs us may be the legacy label, which may live in
			// another domain than the canonical one: resolve it for that label.
			a.label = launchdJobLabel()
			err = a.findDomain()
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "  could not park the service (%v)\n", err)
			return
		}
		signal.Ignore(syscall.SIGTERM)
		// KeepAlive cannot tell this exit from a crash, so the job itself has to
		// go. The plist stays: the next login loads it again. Detached, because
		// bootout terminates this process and must outlive it.
		cmd := exec.Command("launchctl", "bootout", a.target())
		cmd.SysProcAttr = detachedSysProcAttr()
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "  could not park the service (%v)\n", err)
			return
		}
		fmt.Fprintln(os.Stderr, "  Asked launchd to stop the background service until you sign in.")
	case "windows":
		// The shim reads the stop intent after the daemon exits and stays down
		// instead of burning its relaunch attempts on a start that cannot work.
		agent.WriteStopIntent()
		fmt.Fprintln(os.Stderr, "  Background service stopped until you sign in.")
	}
}

// launchdJobLabel is the label of the launchd job running this process, or ""
// when it is not one of ours.
func launchdJobLabel() string {
	switch label := os.Getenv("XPC_SERVICE_NAME"); label {
	case service.LaunchdLabel, service.LegacyLaunchdLabel:
		return label
	}
	return ""
}

// readSelfCgroup returns /proc/self/cgroup, or "" where there is none.
func readSelfCgroup() string {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	return string(data)
}

// runningAsSystemdUnit reports whether a /proc/self/cgroup listing places this
// process in the user unit `unarr daemon install` creates. The cgroup, not an
// environment variable, because INVOCATION_ID is inherited by anything a
// service spawns and does not name the unit; a foreground `unarr start` in a
// terminal sits in a session scope. Only the user manager's subtree counts:
// parkService stops the unit with `systemctl --user`, which cannot address a
// system unit of the same name — and might stop the user's own one instead.
func runningAsSystemdUnit(cgroup string) bool {
	unit := "/" + service.SystemdUnitName + ".service"
	for _, line := range strings.Split(cgroup, "\n") {
		// "<id>:<controllers>:<path>"; the path is the last field.
		i := strings.LastIndexByte(line, ':')
		if i < 0 {
			continue
		}
		path := strings.TrimSpace(line[i+1:])
		if strings.Contains(path, "/user@") && strings.HasSuffix(path, unit) {
			return true
		}
	}
	return false
}

// resumeInstalledService starts the service again after a sign-in, when it
// parked itself for lack of one. A service stopped for any other reason — by
// the user, by the tray — is none of a sign-in's business, and neither is a
// daemon already running outside the service, which re-reads the credential
// itself (a second one would lose the instance lock and restart-loop).
//
// Best-effort: the credential is saved either way, so a failure here is a note
// with the command to run, never an error.
func resumeInstalledService() {
	// A marker under `--config` (a dev agent's config) was never written by the
	// service, which runs with the default one.
	if resolvedConfigPath() != config.FilePath() || !parkedMarkerExists() {
		return
	}
	if st := agent.ReadState(); st != nil && isDaemonAlive(st) {
		clearParkedMarker()
		return
	}
	// Without a download dir the daemon cannot start either, and bringing the
	// service back would only restart the loop parking ended. `unarr init`,
	// which login points the user to, reinstalls and starts it.
	if loadConfig().Download.Dir == "" {
		return
	}
	// Uninstalled (or disabled) since it parked: not a sign-in's to undo.
	if !parkedServiceStillInstalled() {
		clearParkedMarker()
		return
	}
	out, err := startParkedService()
	if err != nil {
		// launchctl errors carry the command's whole output after a newline.
		color.New(color.FgYellow).Printf("  Could not start the background service (%s)\n",
			firstLine(out, errors.New(strings.SplitN(err.Error(), "\n", 2)[0])))
		fmt.Println("  Start it with: unarr daemon start")
		fmt.Println()
		return
	}
	clearParkedMarker()
	color.New(color.FgGreen).Println("  ✓ Background service started")
	fmt.Println()
}

// parkedServiceStillInstalled reports whether the service that parked is still
// there to resume: the unit (enabled), the launchd plist, the scheduled task.
func parkedServiceStillInstalled() bool {
	switch runtime.GOOS {
	case "linux":
		state, _ := svcOutput("systemctl", "--user", "is-enabled", service.SystemdUnitName)
		return service.Respawns() && state == "enabled"
	case "darwin":
		return service.Respawns()
	case "windows":
		return windowsTaskInstalled()
	}
	return false
}

// startParkedService starts the installed service and reports failure with the
// service manager's own output when it has some.
func startParkedService() (string, error) {
	switch runtime.GOOS {
	case "linux":
		out, err := svcOutput("systemctl", "--user", "start", service.SystemdUnitName)
		if err != nil {
			return out, err
		}
		// Type=simple returns as soon as the fork succeeds; give the daemon the
		// same window `daemon install` gives it before calling it up.
		time.Sleep(unitSettleDelay)
		if state, _ := svcOutput("systemctl", "--user", "is-active", service.SystemdUnitName); state != "active" {
			return "", fmt.Errorf("the service did not stay up (is-active: %s); check: journalctl --user -u unarr -n 50", state)
		}
		return "", nil
	case "darwin":
		home, _ := os.UserHomeDir()
		a, err := newLaunchdAgent(home)
		if err != nil {
			return "", err
		}
		return "", a.start() // waits until the job has stayed running
	case "windows":
		// The task itself, never startWindowsDaemon's detached fallback: a task
		// that will not run was disabled, and that is the user's call.
		agent.WriteStartRequest()
		return svcOutput("schtasks", "/run", "/tn", "unarr")
	}
	return "", fmt.Errorf("service control not supported on %s", runtime.GOOS)
}

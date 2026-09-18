package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/service"
	"github.com/Unarr-app/unarr-cli/internal/winproc"
)

// load/unload are legacy commands: load can print "Load failed: 5" and
// exit ZERO. Use explicit domains and verify the resulting process instead.
type launchdAgent struct {
	domain, label, path string
	run                 func(...string) (string, error)
	timeout, settle     time.Duration
}

func launchctlOutput(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "launchctl", args...)
	winproc.HideWindow(cmd)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func newLaunchdAgent(home string) (*launchdAgent, error) {
	if _, err := exec.LookPath("launchctl"); err != nil {
		return nil, fmt.Errorf("launchd is not available here (launchctl not found): %w", err)
	}
	a := &launchdAgent{
		label: service.LaunchdLabel, path: service.PlistPath(home),
		run: launchctlOutput, timeout: 35 * time.Second, settle: detachedStartupProbe,
	}
	if err := a.findDomain(); err != nil {
		return nil, err
	}
	if _, err := os.Stat(a.path); !os.IsNotExist(err) {
		return a, err
	}
	if _, err := os.Stat(service.LegacyPlistPath(home)); os.IsNotExist(err) {
		return a, nil
	}
	// A lost canonical plist does not erase its live registration. Prefer that
	// owner over a second definition left on disk.
	if _, loaded, err := a.status(); err != nil || loaded {
		return a, err
	}
	label, err := legacyPlistLabel(service.LegacyPlistPath(home))
	if errors.Is(err, errUnknownLegacyPlist) {
		// An unrelated or damaged file must not prevent repairing the canonical
		// installation. Cleanup still checks the known legacy job independently.
		return a, nil
	}
	if err != nil {
		return nil, err
	}
	a.label, a.path = label, service.LegacyPlistPath(home)
	if err := a.findDomain(); err != nil {
		return nil, err
	}
	return a, nil
}

// Preserve an existing user-domain registration after a GUI login. For a new
// service prefer the GUI session, falling back to user on an SSH-only machine.
func (a *launchdAgent) findDomain() error {
	var available string
	for _, kind := range []string{"gui/", "user/"} {
		domain := kind + strconv.Itoa(os.Getuid())
		if _, err := a.run("print", domain); err != nil {
			continue
		}
		if available == "" {
			available = domain
		}
		a.domain = domain
		_, loaded, err := a.status()
		if err != nil {
			return err
		}
		if loaded {
			return nil
		}
	}
	if available == "" {
		return fmt.Errorf("no launchd user session available; run this command from your macOS login session")
	}
	a.domain = available
	return nil
}

func (a *launchdAgent) target() string { return a.domain + "/" + a.label }

func (a *launchdAgent) command(args ...string) error {
	out, err := a.run(args...)
	if err != nil {
		return fmt.Errorf("launchctl %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return nil
}

func (a *launchdAgent) status() (string, bool, error) {
	out, err := a.run("print", a.target())
	if err == nil {
		return out, true, nil
	}
	// ESRCH (113 in launchctl's service namespace) means not registered.
	// Permission errors, missing domains and a broken executable are NOT absence.
	var exit interface{ ExitCode() int }
	if errors.As(err, &exit) && exit.ExitCode() == 113 {
		return out, false, nil
	}
	return out, false, fmt.Errorf("query launchd service %s: %w\n%s", a.target(), err, out)
}

func launchdPID(out string) int {
	for _, line := range strings.Split(out, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "pid = "); ok {
			pid, _ := strconv.Atoi(value)
			return pid
		}
	}
	return 0
}

func (a *launchdAgent) start() error {
	if err := a.command("enable", a.target()); err != nil {
		return err
	}
	_, loaded, err := a.status()
	if err != nil {
		return err
	}
	if loaded {
		// Without -k this starts a dead job and leaves a live process alone.
		err = a.command("kickstart", a.target())
	} else {
		if _, err := os.Stat(a.path); err != nil {
			return fmt.Errorf("read launchd plist: %w; run 'unarr daemon install' to repair the service", err)
		}
		err = a.command("bootstrap", a.domain, a.path)
	}
	if err != nil {
		return err
	}
	return a.waitRunning()
}

func (a *launchdAgent) waitRunning() error {
	deadline := time.Now().Add(5*time.Second + a.settle)
	var pid int
	var since time.Time
	var out string
	for time.Now().Before(deadline) {
		current, loaded, err := a.status()
		if err != nil {
			return err
		}
		out = current
		next := launchdPID(out)
		if !loaded || next == 0 || next != pid || !strings.Contains(out, "state = running\n") {
			pid, since = next, time.Now()
		} else if time.Since(since) >= a.settle {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("launchd agent %s did not stay running; run 'unarr logs --boot' and 'unarr logs'\n%s", a.target(), out)
}

func (a *launchdAgent) stop() error {
	out, loaded, err := a.status()
	if err != nil || !loaded {
		return err
	}
	pid := launchdPID(out)
	if err := a.command("bootout", a.target()); err != nil {
		return err
	}
	// bootout can return before termination completes. Reloading immediately
	// races the old registration (error 5) and the daemon's instance lock.
	deadline := time.Now().Add(a.timeout)
	for time.Now().Before(deadline) {
		_, loaded, err := a.status()
		if err != nil {
			return err
		}
		if !loaded && (pid == 0 || !agent.IsProcessAlive(pid)) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("launchd agent %s did not stop within %s; refusing to start a competing daemon", a.target(), a.timeout)
}

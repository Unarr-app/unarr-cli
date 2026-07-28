package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/service"
	"github.com/Unarr-app/unarr-cli/internal/winproc"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

const systemdTemplate = `[Unit]
Description=unarr download daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart={{.BinPath}} start
Restart=always
RestartSec=10
Environment=HOME={{.Home}}

[Install]
WantedBy=default.target
`

const launchdTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.torrentclaw.unarr</string>
  <key>ProgramArguments</key>
  <array>
    <string>{{.BinPath}}</string>
    <string>start</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>{{.LogDir}}/unarr.log</string>
  <key>StandardErrorPath</key>
  <string>{{.LogDir}}/unarr.err.log</string>
</dict>
</plist>
`

func newDaemonInstallCmdReal() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Install daemon as a system service (systemd/launchd)",
		Long: `Install the unarr daemon as a system service so it starts automatically on boot.

  Linux:  Creates a systemd user service (~/.config/systemd/user/unarr.service)
          Enables lingering so the service runs without an active login session.
  macOS:  Creates a launchd user agent (~/Library/LaunchAgents/com.torrentclaw.unarr.plist)

The service is enabled and started immediately after installation.
No sudo or root access is required (uses user-level service managers).`,
		Example: `  unarr daemon install`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonInstall()
		},
	}
}

func newDaemonUninstallCmdReal() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove daemon system service",
		Long: `Stop the daemon and remove the system service created by 'unarr daemon install'.

Removes the service file and disables automatic startup on boot.`,
		Example: `  unarr daemon uninstall`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonUninstall()
		},
	}
}

// noServiceManagerHelp is appended to every install failure. A service manager
// that isn't there is not a bug the user can fix — but "your NAS has no systemd"
// is only useful next to the way that box DOES start things at boot.
const noServiceManagerHelp = `
  Start it another way:
    unarr start                    foreground (Ctrl+C to stop)
    Synology DSM                   Control Panel → Task Scheduler → new triggered task, event "Boot-up"
    unRAID                         add "unarr start &" to /boot/config/go
    anything else with cron        crontab -e → @reboot unarr start
`

// svcOutput runs a service-manager command and returns its combined output, so
// a failure can be reported verbatim instead of silently discarded.
func svcOutput(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	winproc.HideWindow(cmd)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// firstLine picks the most useful one-liner out of a command's output, falling
// back to the error when the command printed nothing.
func firstLine(out string, err error) string {
	if out == "" {
		return err.Error()
	}
	return strings.SplitN(out, "\n", 2)[0]
}

// unitSettleDelay is how long we let a freshly started unit run before deciding
// it came up. It is sized against the unit's own RestartSec=10 only loosely: we
// just need to outlive the "starts, then dies on a bad config" window, not the
// first restart. Kept short so a healthy install still feels instant.
const unitSettleDelay = 3 * time.Second

// unitRestartCount reports how many times systemd has already had to respawn the
// unit. Returns 0 when the property cannot be read (older systemd predating
// NRestarts) — never invent a failure out of a missing field.
func unitRestartCount() int {
	out, err := svcOutput("systemctl", "--user", "show", "unarr", "-p", "NRestarts", "--value")
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0
	}
	return n
}

type serviceData struct {
	BinPath string
	User    string
	Home    string
	LogDir  string
}

// writeServiceFile renders a unit/plist template to path, creating its parent
// directory. Shared by both installers so the file the supervisor reads is
// written exactly one way.
func writeServiceFile(path, tmplText string, data serviceData) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create service dir: %w", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create service file: %w", err)
	}
	defer f.Close()
	if err := template.Must(template.New("service").Parse(tmplText)).Execute(f, data); err != nil {
		return fmt.Errorf("write service file: %w", err)
	}
	return nil
}

func runDaemonInstall() error {
	// Same reason as `unarr init`: a sudo install writes the systemd USER unit
	// (and the config it reads) under /root, where the user's own session never
	// looks for it.
	if err := sudoGuard("daemon install"); err != nil {
		return err
	}

	binPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find executable: %w", err)
	}
	binPath, _ = filepath.EvalSymlinks(binPath)

	home, _ := os.UserHomeDir()
	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("USERNAME")
	}

	data := serviceData{
		BinPath: binPath,
		User:    user,
		Home:    home,
		LogDir:  filepath.Join(home, ".local", "share", "unarr"),
	}

	bold := color.New(color.Bold)
	green := color.New(color.FgGreen)

	fmt.Println()
	bold.Println("  unarr daemon install")
	fmt.Println()

	switch runtime.GOOS {
	case "linux":
		return installSystemd(data, green)
	case "darwin":
		return installLaunchd(data, green)
	case "windows":
		return installWindowsTask(data, green)
	default:
		return fmt.Errorf("service installation not supported on %s yet", runtime.GOOS)
	}
}

func installSystemd(data serviceData, green *color.Color) error {
	// Same path service.Respawns() probes — detection and install must never drift.
	path := service.SystemdUnitPathIn(data.Home)
	if err := writeServiceFile(path, systemdTemplate, data); err != nil {
		return err
	}

	// A unit file left behind by a FAILED install is worse than no file at all:
	// service.Respawns() detects a supervisor purely by this path existing, so
	// `unarr stop` would delegate to `systemctl --user stop` on a box that has no
	// user bus — erroring, or printing "✓ Stopped" while the detached daemon
	// keeps running. Every error path below must take the file with it.
	installed := false
	defer func() {
		if installed {
			return
		}
		os.Remove(path)
		// The unit may already be enabled at this point; removing only the file
		// would leave a dangling wants/ symlink that makes every later
		// daemon-reload warn. Both calls are no-ops when we never got that far.
		disableCmd := exec.Command("systemctl", "--user", "disable", service.SystemdUnitName)
		winproc.HideWindow(disableCmd)
		disableCmd.Run()
		reloadCmd := exec.Command("systemctl", "--user", "daemon-reload")
		winproc.HideWindow(reloadCmd)
		reloadCmd.Run()
	}()

	fmt.Printf("  Created: %s\n", path)

	// systemd is not a given: NAS firmwares (Synology, QNAP, unRAID), containers
	// and minimal distros have no user manager at all. Discarding these errors
	// printed a green "Installed and started!" over a unit file nothing would
	// ever run — the worst kind of dead end, because the user has no reason to
	// look further.
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("systemd is not available here (systemctl not found).\n%s", noServiceManagerHelp)
	}
	for _, args := range [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", "unarr"},
		{"--user", "start", "unarr"},
	} {
		if out, err := svcOutput("systemctl", args...); err != nil {
			return fmt.Errorf("systemctl %s failed: %w\n  %s\n%s",
				strings.Join(args, " "), err, out, noServiceManagerHelp)
		}
	}

	// Enable lingering so user services run without login session. Best-effort:
	// loginctl is missing on some systemd-lite systems and the service still
	// works while the user is logged in, so warn rather than fail.
	if out, err := svcOutput("loginctl", "enable-linger", data.User); err != nil {
		color.New(color.FgYellow).Printf("  Note: could not enable lingering (%s) — the service may stop when you log out\n", firstLine(out, err))
	}

	// enable+start exiting 0 is not proof the unit came up: Type=simple makes
	// `systemctl start` return the moment the fork succeeds, so a daemon that
	// dies 200ms later still reads "active" when polled at t=0. Wait out one
	// RestartSec-sized window before asking, and require a plain "active" —
	// with Restart=always, "activating" at this point is the crash-loop
	// signature, not a slow boot.
	time.Sleep(unitSettleDelay)
	state, stateErr := svcOutput("systemctl", "--user", "is-active", "unarr")
	if state != "active" {
		return fmt.Errorf("the unarr service did not come up (is-active: %s).\n  Check: journalctl --user -u unarr -n 50\n%s",
			firstLine(state, stateErr), noServiceManagerHelp)
	}
	// Restart=always also means a unit that keeps dying reads "active" on any
	// single poll — it was just respawned. The restart counter is the only
	// honest signal that ExecStart is failing.
	if n := unitRestartCount(); n > 0 {
		return fmt.Errorf("the unarr service keeps restarting (%d restart(s) in its first seconds) — it starts and exits.\n  Check: journalctl --user -u unarr -n 50\n%s",
			n, noServiceManagerHelp)
	}

	installed = true

	fmt.Println()
	green.Println("  ✓ Installed and started!")
	fmt.Println()
	fmt.Println("  Manage with:")
	fmt.Println("    systemctl --user status unarr")
	fmt.Println("    systemctl --user restart unarr")
	fmt.Println("    journalctl --user -u unarr -f")
	fmt.Println()

	return nil
}

func installLaunchd(data serviceData, green *color.Color) error {
	os.MkdirAll(data.LogDir, 0o755)

	path := service.PlistPath(data.Home)
	if err := writeServiceFile(path, launchdTemplate, data); err != nil {
		return err
	}

	// Same reasoning as the systemd unit: service.Respawns() treats this plist's
	// existence as "a supervisor owns the daemon", so a plist that outlives a
	// failed install makes `unarr stop` unload an agent that was never loaded.
	installed := false
	defer func() {
		if !installed {
			os.Remove(path)
		}
	}()

	fmt.Printf("  Created: %s\n", path)

	// Same reasoning as systemd: a discarded launchctl error printed a green
	// check over a plist nothing had loaded.
	if _, err := exec.LookPath("launchctl"); err != nil {
		return fmt.Errorf("launchd is not available here (launchctl not found).\n%s", noServiceManagerHelp)
	}
	// `launchctl load` exits non-zero when the label is already bootstrapped, so
	// on a healthy machine a re-install would hard-fail. Unload first; its error
	// when nothing is loaded (the normal first install) is expected, not a
	// problem, so it is deliberately discarded.
	_, _ = svcOutput("launchctl", "unload", path)
	if out, err := svcOutput("launchctl", "load", path); err != nil {
		return fmt.Errorf("launchctl load failed: %w\n  %s\n%s", err, out, noServiceManagerHelp)
	}
	if out, err := svcOutput("launchctl", "list", service.LaunchdLabel); err != nil {
		return fmt.Errorf("the unarr agent did not load (launchctl list: %s).\n  Check: %s\n%s",
			firstLine(out, err), filepath.Join(data.LogDir, "unarr.err.log"), noServiceManagerHelp)
	}

	installed = true

	fmt.Println()
	green.Println("  ✓ Installed and loaded!")
	fmt.Println()
	fmt.Println("  Manage with:")
	fmt.Println("    launchctl list | grep unarr")
	fmt.Println("    launchctl unload " + path)
	fmt.Println("    tail -f " + filepath.Join(data.LogDir, "unarr.log"))
	fmt.Println()

	return nil
}

func runDaemonUninstall() error {
	home, _ := os.UserHomeDir()

	bold := color.New(color.Bold)
	green := color.New(color.FgGreen)

	fmt.Println()
	bold.Println("  unarr daemon uninstall")
	fmt.Println()

	switch runtime.GOOS {
	case "linux":
		stopCmd := exec.Command("systemctl", "--user", "stop", "unarr")
		winproc.HideWindow(stopCmd)
		stopCmd.Run()
		disableCmd := exec.Command("systemctl", "--user", "disable", "unarr")
		winproc.HideWindow(disableCmd)
		disableCmd.Run()
		path := service.SystemdUnitPathIn(home)
		os.Remove(path)
		reloadCmd := exec.Command("systemctl", "--user", "daemon-reload")
		winproc.HideWindow(reloadCmd)
		reloadCmd.Run()
		green.Printf("  ✓ Removed %s\n", path)

	case "darwin":
		path := service.PlistPath(home)
		unloadCmd := exec.Command("launchctl", "unload", path)
		winproc.HideWindow(unloadCmd)
		unloadCmd.Run()
		os.Remove(path)
		green.Printf("  ✓ Removed %s\n", path)

	case "windows":
		// Stop the running process if any
		if state := agent.ReadState(); state != nil {
			killCmd := exec.Command("taskkill", "/pid", strconv.Itoa(state.PID), "/f")
			winproc.HideWindow(killCmd)
			killCmd.Run()
		}
		delCmd := exec.Command("schtasks", "/delete", "/tn", "unarr", "/f")
		winproc.HideWindow(delCmd)
		out, err := delCmd.CombinedOutput()
		if err != nil && !strings.Contains(string(out), "cannot find") {
			return fmt.Errorf("remove scheduled task: %w\n%s", err, strings.TrimSpace(string(out)))
		}
		// Drop the launcher shim the task pointed at. Best-effort: a missing file
		// (never installed, or already cleaned) is not an uninstall failure.
		os.Remove(filepath.Join(config.DataDir(), launcherVBSName))
		green.Println("  ✓ Scheduled task removed")

	default:
		return fmt.Errorf("service uninstall not supported on %s yet", runtime.GOOS)
	}

	fmt.Println()
	return nil
}

func installWindowsTask(data serviceData, green *color.Color) error {
	logDir := config.DataDir()
	os.MkdirAll(logDir, 0o755)

	// Remove any existing task before (re)installing.
	delCmd := exec.Command("schtasks", "/delete", "/tn", "unarr", "/f")
	winproc.HideWindow(delCmd)
	delCmd.Run()

	// Register from an XML definition rather than the flag form of
	// `schtasks /create`. The CLI flags cannot express the three settings that
	// make login start-up reliable, and their absence was the root cause of
	// "sometimes it doesn't start at login":
	//   * <Delay> on the logon trigger — at logon the network/VPN stack may not
	//     be up yet; without a delay `unarr start` races it, fails to reach the
	//     server and exits.
	//   * RestartOnFailure — the flag-form task is a one-shot with NO supervisor
	//     (unlike systemd Restart=always / launchd KeepAlive), so a transient
	//     early exit left the agent dead until the user manually resumed it.
	//   * StartWhenAvailable — recover a missed trigger (asleep/off at logon).
	xmlPath := filepath.Join(logDir, "unarr-task.xml")
	if err := os.WriteFile(xmlPath, buildWindowsTaskXMLBytes(data, logDir), 0o600); err != nil {
		return fmt.Errorf("write task definition: %w", err)
	}

	// The task action launches this VBScript shim via wscript.exe (GUI-subsystem)
	// so the console-subsystem daemon starts with no console window at logon —
	// the boot flash a hidden-PowerShell wrapper could not prevent. Must exist
	// before the task runs (schtasks /run below fires immediately). Written as
	// UTF-16LE+BOM: Windows Script Host decodes a BOM-less .vbs via the ANSI code
	// page, which would corrupt a non-ASCII username in the embedded paths and
	// silently stop the daemon starting at logon (same encoding lesson as the
	// task XML).
	vbsPath := filepath.Join(logDir, launcherVBSName)
	if err := os.WriteFile(vbsPath, buildLauncherVBSBytes(data.BinPath, logDir), 0o600); err != nil {
		return fmt.Errorf("write launcher script: %w", err)
	}

	// With /xml, both the run level (<RunLevel>LeastPrivilege</RunLevel>) and the
	// identity (<Principal><UserId>…</UserId><LogonType>InteractiveToken) live
	// INSIDE the XML. Passing /rl makes schtasks reject the call ("/RL option can
	// only be used with ..."), which is what silently broke task creation. We
	// also DON'T pass /ru: with /xml it's redundant (the Principal carries the
	// user), and on a domain / non-current account /ru with no /rp can block on
	// an interactive password prompt (svcOutput has no stdin) — the XML's
	// InteractiveToken avoids that entirely.
	out, err := svcOutput("schtasks",
		"/create",
		"/tn", "unarr",
		"/xml", xmlPath,
		"/f",
	)
	if err != nil {
		return fmt.Errorf("create scheduled task: %w\n%s", err, out)
	}

	// Run it now. "Installed, will start at next login" is a dead end for
	// someone who just finished the wizard and expects a working agent.
	started := true
	if out, err := svcOutput("schtasks", "/run", "/tn", "unarr"); err != nil {
		started = false
		color.New(color.FgYellow).Printf("  Note: could not start the task now (%s)\n", firstLine(out, err))
	}

	fmt.Println()
	if started {
		green.Println("  ✓ Installed and started! It will also start automatically at login.")
	} else {
		green.Println("  ✓ Installed! It will start automatically at your next login.")
		fmt.Println()
		fmt.Println("  To start now:")
		fmt.Println("    unarr daemon start")
	}
	fmt.Println()
	fmt.Println("  Manage with:")
	fmt.Println("    unarr daemon status")
	fmt.Println("    unarr daemon stop")
	fmt.Printf("    unarr daemon logs          (log: %s\\unarr.log)\n", logDir)
	fmt.Println()

	return nil
}

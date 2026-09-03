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

// launchdTemplate hands the daemon its own log (--log-file unarr.log) and
// leaves launchd holding unarr.boot.log.
//
// The two files have different owners on purpose. The daemon opens unarr.log
// O_APPEND and rotates it by renaming it aside — the only rotation that
// actually shrinks a live file. launchd opens Standard*Path itself and holds it
// for the agent's whole life, so that file can only be trimmed from the outside
// by copy-truncate (which works here: launchd's handle is a genuine POSIX
// append fd); it is deliberately small and collects only what bypasses
// log.SetOutput — the start banner, a fatal cobra error, a panic dump.
//
// stdout and stderr still point at ONE file. Splitting them was a trap: the
// daemon logs through log.Printf, which writes to stderr, so a separate
// StandardErrorPath collected essentially the whole log while `unarr logs` read
// a unarr.log holding only the start banner.
//
// An installed plist is NOT rewritten by a self-update, so a macOS box that
// upgrades its binary keeps the old plist, passes no --log-file and stays on
// copy-truncate — which works on macOS. That is an acceptable steady state, not
// a reason to force a plist rewrite.
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
    <string>--log-file</string>
    <string>{{.LogDir}}/unarr.log</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>{{.LogDir}}/unarr.boot.log</string>
  <key>StandardErrorPath</key>
  <string>{{.LogDir}}/unarr.boot.log</string>
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
//
// It renders into a temporary file in the same directory and renames it into
// place, so path either holds the previous service definition or the new one —
// never a half-written one. The previous shape opened path with os.Create,
// which TRUNCATES: a reinstall over a working install destroyed the unit/plist
// before knowing the template would render, and a failure there (or a crash, or
// a full disk) left an empty file behind. systemd then refuses to load the unit
// and launchd rejects the plist, so the daemon does not come back — and the
// user's next `unarr daemon install` is the only thing that can repair it.
//
// Same directory on purpose: a temp file under /tmp could be on another
// filesystem and the rename would fail with EXDEV.
func writeServiceFile(path, tmplText string, data serviceData) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create service dir: %w", err)
	}

	tmpl, err := template.New("service").Parse(tmplText)
	if err != nil {
		return fmt.Errorf("parse service template: %w", err)
	}

	f, err := os.CreateTemp(dir, ".unarr-service-*")
	if err != nil {
		return fmt.Errorf("create service file: %w", err)
	}
	tmp := f.Name()
	// Every path below either renames tmp away or must not leave it behind.
	defer os.Remove(tmp)

	if err := tmpl.Execute(f, data); err != nil {
		f.Close()
		return fmt.Errorf("write service file: %w", err)
	}
	// Checked, not deferred: a full disk reports itself on Close, and a service
	// file that is silently short is exactly what this function exists to avoid.
	if err := f.Close(); err != nil {
		return fmt.Errorf("write service file: %w", err)
	}
	// CreateTemp makes the file 0600; the supervisor has to be able to read it.
	if err := os.Chmod(tmp, 0o644); err != nil {
		return fmt.Errorf("write service file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("install service file: %w", err)
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

	data, err := resolveServiceData()
	if err != nil {
		return err
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
	// launchd opens StandardOutPath itself and holds it for the life of the
	// agent, so an oversized boot log has to be trimmed here, before `launchctl
	// load` below. From then on the daemon's own janitor keeps it bounded. The
	// main log is trimmed too: an install that predates --log-file may have left
	// one over budget, and this is the same free gap for it.
	rotateDaemonLogIn(data.LogDir)
	rotateBootLogIn(data.LogDir)

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
			firstLine(out, err), filepath.Join(data.LogDir, logFileName), noServiceManagerHelp)
	}

	installed = true

	fmt.Println()
	green.Println("  ✓ Installed and loaded!")
	fmt.Println()
	fmt.Println("  Manage with:")
	fmt.Println("    launchctl list | grep unarr")
	fmt.Println("    launchctl unload " + path)
	fmt.Println("    unarr logs -f              (" + filepath.Join(data.LogDir, logFileName) + ")")
	fmt.Println("    unarr logs --boot          (" + filepath.Join(data.LogDir, bootLogFileName) + ", startup + crashes)")
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
		// Stop the running process if any. Mark the stop as deliberate first, so
		// the launcher shim does not report a failure the (still-present) task
		// would act on in the window before /delete lands — and reap the state
		// file after, so the tray does not read the uninstall as a crash and mail
		// a report for it.
		if state := agent.ReadState(); state != nil {
			agent.WriteStopIntent()
			killCmd := exec.Command("taskkill", "/pid", strconv.Itoa(state.PID), "/f")
			winproc.HideWindow(killCmd)
			killCmd.Run()
			reapStateAfterExit(state.PID)
		}
		delCmd := exec.Command("schtasks", "/delete", "/tn", "unarr", "/f")
		winproc.HideWindow(delCmd)
		out, err := delCmd.CombinedOutput()
		if err != nil && !strings.Contains(string(out), "cannot find") {
			return fmt.Errorf("remove scheduled task: %w\n%s", err, strings.TrimSpace(string(out)))
		}
		// Drop the launcher shim the task pointed at, and the stop-intent marker
		// only it reads. Best-effort: a missing file (never installed, or already
		// cleaned) is not an uninstall failure. Removed only after the task is
		// gone, so the shim still sees the marker while it is deciding its exit
		// code.
		os.Remove(filepath.Join(config.DataDir(), launcherVBSName))
		os.Remove(agent.StopIntentPath())
		// Leave nothing of ours behind in the firewall. Best-effort by design:
		// the rules may never have been created (non-elevated install).
		removeWindowsFirewallRules()
		green.Println("  ✓ Scheduled task removed")

	default:
		return fmt.Errorf("service uninstall not supported on %s yet", runtime.GOOS)
	}

	fmt.Println()
	return nil
}

// writeAndCreateWindowsTask writes the launcher shim + task XML and registers
// the scheduled task, WITHOUT running it or printing the install wizard. It is
// the idempotent core shared by a fresh `daemon install` and the post-upgrade
// re-registration (reregisterWindowsTaskAfterUpgrade) — the latter needs to
// rewrite an already-installed task to point at the new launcher, but must not
// start a second daemon or print "Installed!". `schtasks /create /f` overwrites
// any existing task, so calling this on an already-installed box is safe.
func writeAndCreateWindowsTask(data serviceData, logDir string) error {
	os.MkdirAll(logDir, 0o755)
	// The shim redirects with `>> unarr.boot.log`, so cmd.exe — not us — owns
	// THAT handle once the task runs; unarr.log is the daemon's own. Trim both
	// now, while nothing holds either: from here the daemon's Writer rotates
	// unarr.log by rename and the shim size-checks the boot log before each
	// relaunch (cmd.exe grants only FILE_SHARE_READ, so copy-truncate can never
	// bound the boot log while it is held — measured on the VM harness).
	//
	// All of that is OPT-IN: with the default log_max_size_mb = 0 both trims are
	// no-ops and the shim is generated without a trim at all (bootLogTrimBytes).
	//
	// "While nothing holds either" is now CHECKED rather than assumed: on the
	// self-update path this runs BEFORE the daemon is restarted, so the old
	// daemon is still writing unarr.log, and rotateDaemonLogIn correctly does
	// nothing. The trim is not lost — the restarted daemon seeds its Writer from
	// the file on disk, so its very first line (the run marker) rotates an
	// over-budget log by rename, from the inside, where it is safe.
	rotateDaemonLogIn(logDir)
	rotateBootLogIn(logDir)

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
	if err := os.WriteFile(vbsPath, buildLauncherVBSBytes(data.BinPath, logDir, bootLogTrimBytes()), 0o600); err != nil {
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
	if out, err := svcOutput("schtasks",
		"/create",
		"/tn", "unarr",
		"/xml", xmlPath,
		"/f",
	); err != nil {
		return fmt.Errorf("create scheduled task: %w\n%s", err, out)
	}
	return nil
}

func installWindowsTask(data serviceData, green *color.Color) error {
	logDir := config.DataDir()

	if err := writeAndCreateWindowsTask(data, logDir); err != nil {
		return err
	}

	// Inbound peer traffic. Without it far fewer peers reach us and magnet
	// downloads fail as "no peers found" on healthy swarms — the 58% Windows
	// failure rate measured on 2026-09-03. Scoped to the binary, not to a port:
	// see daemon_install_winfirewall.go for why. Never fatal.
	addWindowsFirewallRules(data.BinPath, green)

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
	fmt.Printf("    unarr daemon logs          (log: %s)\n", filepath.Join(logDir, logFileName))
	fmt.Printf("    unarr logs --boot          (startup + crashes: %s)\n", filepath.Join(logDir, bootLogFileName))
	fmt.Println()

	return nil
}

// resolveServiceData resolves the current executable, user, and home into the
// serviceData every installer needs. Shared by `daemon install` and the
// post-upgrade task re-registration so both point the service at the SAME
// resolved binary path.
func resolveServiceData() (serviceData, error) {
	binPath, err := os.Executable()
	if err != nil {
		return serviceData{}, fmt.Errorf("find executable: %w", err)
	}
	binPath, _ = filepath.EvalSymlinks(binPath)

	home, _ := os.UserHomeDir()
	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("USERNAME")
	}
	return serviceData{
		BinPath: binPath,
		User:    user,
		Home:    home,
		// The SAME directory every reader and writer resolves — hardcoding the
		// Linux XDG path here pointed the launchd plist at ~/.local/share/unarr
		// on macOS while `unarr logs`, the janitor and `clean` all looked in
		// ~/Library/Application Support/unarr: the log grew unsupervised in one
		// place and "no daemon log yet" was printed about the other.
		LogDir: config.DataDir(),
	}, nil
}

// windowsTaskInstalled reports whether the `unarr` scheduled task exists.
// schtasks /query exits non-zero ("ERROR: The system cannot find the ... task")
// when it does not, which is how we distinguish "user runs at logon" (rewrite
// it) from "user never installed autostart" (leave them alone).
func windowsTaskInstalled() bool {
	cmd := exec.Command("schtasks", "/query", "/tn", "unarr")
	winproc.HideWindow(cmd)
	return cmd.Run() == nil
}

// reregisterWindowsTaskAfterUpgrade rewrites an EXISTING Windows autostart task
// to point at the current binary's launcher, then returns whether it did so.
//
// Why this exists: `unarr update` swaps the binary in place but does NOT touch
// the scheduled task. A user who installed autostart on an older build (whose
// task launched the console daemon through `powershell -WindowStyle Hidden`, or
// directly) and then self-updated keeps that stale task — so the boot console
// flash the newer launcher fixes STILL happens, because the task never learned
// about the new wscript/VBS launcher. This closes that gap: after a successful
// upgrade we re-register the task (idempotently) so it adopts the new launcher
// without the user having to run `daemon uninstall && daemon install` by hand.
//
// It only acts when a task already exists (never installs autostart the user
// didn't ask for) and is best-effort — a failure here must not fail an upgrade
// whose binary is already on disk. Returns (rewrote, err).
func reregisterWindowsTaskAfterUpgrade() (bool, error) {
	if runtime.GOOS != "windows" {
		return false, nil
	}
	if !windowsTaskInstalled() {
		return false, nil
	}
	data, err := resolveServiceData()
	if err != nil {
		return false, err
	}
	// Rewrite the shim + task to point at the new binary. Does NOT /run it: the
	// upgrade path already restarts a running daemon separately, and a fresh
	// logon will pick up the rewritten task. `schtasks /create /f` overwrites.
	if err := writeAndCreateWindowsTask(data, config.DataDir()); err != nil {
		return false, err
	}
	return true, nil
}

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

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/torrentclaw/unarr/internal/agent"
	"github.com/torrentclaw/unarr/internal/config"
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

type serviceData struct {
	BinPath string
	User    string
	Home    string
	LogDir  string
}

func runDaemonInstall() error {
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
	// User-level systemd service (no sudo needed)
	dir := filepath.Join(data.Home, ".config", "systemd", "user")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create systemd dir: %w", err)
	}

	path := filepath.Join(dir, "unarr.service")
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create service file: %w", err)
	}
	defer f.Close()

	tmpl := template.Must(template.New("systemd").Parse(systemdTemplate))
	if err := tmpl.Execute(f, data); err != nil {
		return fmt.Errorf("write service file: %w", err)
	}

	fmt.Printf("  Created: %s\n", path)

	// Enable and start
	exec.Command("systemctl", "--user", "daemon-reload").Run()
	exec.Command("systemctl", "--user", "enable", "unarr").Run()
	exec.Command("systemctl", "--user", "start", "unarr").Run()

	// Enable lingering so user services run without login session
	exec.Command("loginctl", "enable-linger", data.User).Run()

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

	dir := filepath.Join(data.Home, "Library", "LaunchAgents")
	os.MkdirAll(dir, 0o755)

	path := filepath.Join(dir, "com.torrentclaw.unarr.plist")
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create plist: %w", err)
	}
	defer f.Close()

	tmpl := template.Must(template.New("launchd").Parse(launchdTemplate))
	if err := tmpl.Execute(f, data); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}

	fmt.Printf("  Created: %s\n", path)

	exec.Command("launchctl", "load", path).Run()

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
		exec.Command("systemctl", "--user", "stop", "unarr").Run()
		exec.Command("systemctl", "--user", "disable", "unarr").Run()
		path := filepath.Join(home, ".config", "systemd", "user", "unarr.service")
		os.Remove(path)
		exec.Command("systemctl", "--user", "daemon-reload").Run()
		green.Printf("  ✓ Removed %s\n", path)

	case "darwin":
		path := filepath.Join(home, "Library", "LaunchAgents", "com.torrentclaw.unarr.plist")
		exec.Command("launchctl", "unload", path).Run()
		os.Remove(path)
		green.Printf("  ✓ Removed %s\n", path)

	case "windows":
		// Stop the running process if any
		if state := agent.ReadState(); state != nil {
			exec.Command("taskkill", "/pid", strconv.Itoa(state.PID), "/f").Run()
		}
		out, err := exec.Command("schtasks", "/delete", "/tn", "unarr", "/f").CombinedOutput()
		if err != nil && !strings.Contains(string(out), "cannot find") {
			return fmt.Errorf("remove scheduled task: %w\n%s", err, strings.TrimSpace(string(out)))
		}
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
	exec.Command("schtasks", "/delete", "/tn", "unarr", "/f").Run()

	// Wrap with PowerShell so stdout/stderr are captured to a log file.
	psScript := fmt.Sprintf(
		`Start-Transcript -Path '%s\unarr.log' -Append -NoClobber; & '%s' start`,
		logDir, data.BinPath,
	)
	taskCmd := fmt.Sprintf(`powershell.exe -NonInteractive -WindowStyle Hidden -Command "%s"`, psScript)

	out, err := exec.Command("schtasks",
		"/create",
		"/tn", "unarr",
		"/tr", taskCmd,
		"/sc", "onlogon",
		"/ru", data.User,
		"/rl", "highest",
		"/f",
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("create scheduled task: %w\n%s", err, strings.TrimSpace(string(out)))
	}

	fmt.Println()
	green.Println("  ✓ Installed! Service will start automatically at next login.")
	fmt.Println()
	fmt.Println("  To start now:")
	fmt.Println("    unarr daemon start")
	fmt.Println()
	fmt.Println("  Manage with:")
	fmt.Println("    unarr daemon status")
	fmt.Println("    unarr daemon stop")
	fmt.Printf("    unarr daemon logs          (log: %s\\unarr.log)\n", logDir)
	fmt.Println()

	return nil
}

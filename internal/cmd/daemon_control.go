package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/torrentclaw/unarr/internal/agent"
	"github.com/torrentclaw/unarr/internal/config"
)

func newDaemonStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the installed daemon service",
		Long: `Start the unarr daemon using the system service manager.
Requires 'unarr daemon install' to have been run first.

  Linux:   systemctl --user start unarr
  macOS:   launchctl load ~/Library/LaunchAgents/com.torrentclaw.unarr.plist
  Windows: schtasks /run /tn unarr`,
		Example: `  unarr daemon start`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonSvcStart()
		},
	}
}

func newDaemonStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the running daemon service",
		Long: `Stop the unarr daemon service.

  Linux:   systemctl --user stop unarr
  macOS:   launchctl unload ~/Library/LaunchAgents/com.torrentclaw.unarr.plist
  Windows: sends stop signal via process PID`,
		Example: `  unarr daemon stop`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonSvcStop()
		},
	}
}

func newDaemonRestartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Restart the daemon service",
		Long: `Restart the unarr daemon service.

  Linux:   systemctl --user restart unarr
  macOS:   unload + reload launchd agent
  Windows: stop by PID + schtasks /run`,
		Example: `  unarr daemon restart`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonSvcRestart()
		},
	}
}

func newDaemonSvcStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show daemon service status",
		Long: `Show the current status of the unarr daemon service as reported
by the system service manager, plus local state information.`,
		Example: `  unarr daemon status`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonSvcStatus()
		},
	}
}

func newDaemonLogsCmd() *cobra.Command {
	var follow bool
	var lines int

	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Show daemon logs",
		Long: `Show daemon log output.

  Linux:   streams from journald (journalctl --user -u unarr)
  macOS:   tails ~/.local/share/unarr/unarr.log
  Windows: tails %LOCALAPPDATA%\unarr\unarr.log`,
		Example: `  unarr daemon logs
  unarr daemon logs -f
  unarr daemon logs -n 100 -f`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonLogs(follow, lines)
		},
	}

	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow log output")
	cmd.Flags().IntVarP(&lines, "lines", "n", 50, "Number of lines to show")
	return cmd
}

func newDaemonReloadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reload",
		Short: "Reload daemon configuration without restarting",
		Long: `Send a reload signal to the running daemon, causing it to
re-read its configuration file without interrupting active downloads.

  Linux/macOS: sends SIGUSR1 to the daemon process
  Windows:     not supported (use 'unarr daemon restart' instead)`,
		Example: `  unarr daemon reload`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonReload()
		},
	}
}

// ── Platform implementations ──────────────────────────────────────────────────

func runDaemonSvcStart() error {
	fmt.Println()
	switch runtime.GOOS {
	case "linux":
		if err := svcExec("systemctl", "--user", "start", "unarr"); err != nil {
			fmt.Fprintln(os.Stderr, "\n  Is the daemon installed? Run 'unarr daemon install' first.")
			return fmt.Errorf("start service: %w", err)
		}
	case "darwin":
		home, _ := os.UserHomeDir()
		plist := launchdPlistPath(home)
		if _, err := os.Stat(plist); err != nil {
			return fmt.Errorf("service not installed — run 'unarr daemon install' first")
		}
		if err := svcExec("launchctl", "load", plist); err != nil {
			return fmt.Errorf("load service: %w", err)
		}
	case "windows":
		if err := svcExec("schtasks", "/run", "/tn", "unarr"); err != nil {
			fmt.Fprintln(os.Stderr, "\n  Is the daemon installed? Run 'unarr daemon install' first.")
			return fmt.Errorf("start task: %w", err)
		}
	default:
		return fmt.Errorf("service control not supported on %s", runtime.GOOS)
	}

	color.New(color.FgGreen).Println("  ✓ Started")
	fmt.Println()
	return nil
}

func runDaemonSvcStop() error {
	fmt.Println()
	switch runtime.GOOS {
	case "linux":
		if err := svcExec("systemctl", "--user", "stop", "unarr"); err != nil {
			return fmt.Errorf("stop service: %w", err)
		}
	case "darwin":
		home, _ := os.UserHomeDir()
		plist := launchdPlistPath(home)
		if err := svcExec("launchctl", "unload", plist); err != nil {
			return fmt.Errorf("unload service: %w", err)
		}
	default:
		return stopDaemonByPID()
	}

	color.New(color.FgGreen).Println("  ✓ Stopped")
	fmt.Println()
	return nil
}

func runDaemonSvcRestart() error {
	switch runtime.GOOS {
	case "linux":
		fmt.Println()
		if err := svcExec("systemctl", "--user", "restart", "unarr"); err != nil {
			return fmt.Errorf("restart service: %w", err)
		}
		color.New(color.FgGreen).Println("  ✓ Restarted")
		fmt.Println()
		return nil
	default:
		fmt.Println("  Stopping...")
		_ = runDaemonSvcStop()
		fmt.Println("  Starting...")
		return runDaemonSvcStart()
	}
}

func runDaemonSvcStatus() error {
	fmt.Println()
	switch runtime.GOOS {
	case "linux":
		// systemctl gives rich formatted output; exit code non-zero when stopped is fine.
		svcExec("systemctl", "--user", "status", "--no-pager", "unarr") //nolint:errcheck
	case "darwin":
		printDaemonStatusDarwin()
	case "windows":
		svcExec("schtasks", "/query", "/tn", "unarr", "/fo", "LIST") //nolint:errcheck
	default:
		fmt.Printf("  Service manager not supported on %s\n", runtime.GOOS)
	}

	printStateInfo()
	return nil
}

func runDaemonLogs(follow bool, lines int) error {
	switch runtime.GOOS {
	case "linux":
		args := []string{"--user", "-u", "unarr", "--no-pager", "-n", strconv.Itoa(lines)}
		if follow {
			// -f implies live output; drop --no-pager so journalctl can control the terminal.
			args = []string{"--user", "-u", "unarr", "-f"}
		}
		return svcExecInteractive("journalctl", args...)

	case "darwin":
		home, _ := os.UserHomeDir()
		logFile := filepath.Join(home, ".local", "share", "unarr", "unarr.log")
		if _, err := os.Stat(logFile); err != nil {
			fmt.Fprintln(os.Stderr, "The daemon writes this file when running as a launchd service. Run 'unarr daemon install' first.")
			return fmt.Errorf("log file not found: %s", logFile)
		}
		args := []string{"-n", strconv.Itoa(lines)}
		if follow {
			args = append(args, "-f")
		}
		args = append(args, logFile)
		return svcExecInteractive("tail", args...)

	case "windows":
		logFile := filepath.Join(config.DataDir(), "unarr.log")
		if _, err := os.Stat(logFile); err != nil {
			fmt.Fprintln(os.Stderr, "The daemon writes logs here when running. Start it first.")
			return fmt.Errorf("log file not found: %s", logFile)
		}
		var psCmd string
		if follow {
			psCmd = fmt.Sprintf("Get-Content -Path '%s' -Tail %d -Wait", logFile, lines)
		} else {
			psCmd = fmt.Sprintf("Get-Content -Path '%s' -Tail %d", logFile, lines)
		}
		return svcExecInteractive("powershell", "-NonInteractive", "-Command", psCmd)

	default:
		return fmt.Errorf("log viewing not supported on %s", runtime.GOOS)
	}
}

func runDaemonReload() error {
	return sendReloadSignal()
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// stopDaemonByPID reads the state file and sends a graceful stop to the daemon PID.
// Used as fallback on platforms without a service manager (and as Windows implementation).
func stopDaemonByPID() error {
	state := agent.ReadState()
	if state == nil {
		return fmt.Errorf("daemon does not appear to be running (state file not found)")
	}
	return killPID(state.PID)
}

func launchdPlistPath(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", "com.torrentclaw.unarr.plist")
}

// printDaemonStatusDarwin shows launchd service state by filtering launchctl output.
func printDaemonStatusDarwin() {
	out, err := exec.Command("launchctl", "list").Output()
	if err != nil {
		fmt.Printf("  Could not query launchctl: %v\n", err)
		return
	}
	found := false
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "unarr") {
			// Format: PID  ExitCode  Label
			fmt.Printf("  launchd: %s\n", strings.TrimSpace(line))
			found = true
		}
	}
	if !found {
		fmt.Println("  launchd: service not loaded")
	}
}

// printStateInfo shows information from the local daemon.state.json file.
func printStateInfo() {
	state := agent.ReadState()
	if state == nil {
		color.New(color.FgHiBlack).Println("  State:   no state file (daemon not running or crashed)")
		fmt.Println()
		return
	}
	dim := color.New(color.FgHiBlack)
	fmt.Println()
	dim.Println("  Local state:")
	fmt.Printf("    PID:        %d\n", state.PID)
	fmt.Printf("    Status:     %s\n", state.Status)
	fmt.Printf("    Version:    %s\n", state.Version)
	fmt.Printf("    Uptime:     %s\n", formatDuration(time.Since(state.StartedAt)))
	fmt.Printf("    Heartbeat:  %s ago\n", formatDuration(time.Since(state.LastHeartbeat)))
	fmt.Printf("    Active:     %d task(s)\n", state.ActiveTasks)
	fmt.Println()
}

// svcExec runs a service management command with output flowing to the terminal.
func svcExec(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// svcExecInteractive is like svcExec but also connects stdin (needed for follow/pager modes).
func svcExecInteractive(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

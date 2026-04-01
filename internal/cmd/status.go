package cmd

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/torrentclaw/unarr/internal/agent"
	"github.com/torrentclaw/unarr/internal/upgrade"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show daemon status, configuration, and update availability",
		Long: `Display the current state of unarr: version, configuration, daemon status,
disk usage, and whether an update is available.

When the daemon is running, also displays uptime, active downloads, and stats.`,
		Example: `  unarr status`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatus()
		},
	}
}

func runStatus() error {
	bold := color.New(color.Bold)
	dim := color.New(color.FgHiBlack)
	green := color.New(color.FgGreen)
	yellow := color.New(color.FgYellow)
	cyan := color.New(color.FgCyan)

	fmt.Println()
	bold.Printf("  unarr %s\n", Version)
	dim.Printf("  %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Println()

	cfg := loadConfig()

	// ── Configuration ──
	if cfg.Auth.APIKey == "" {
		yellow.Println("  ⚠  Not configured. Run 'unarr init' first.")
		fmt.Println()
		return nil
	}

	// ── Account (async fetch) ──
	type accountResult struct {
		user agent.UserInfo
		err  error
	}
	accountCh := make(chan accountResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		ac := agent.NewClient(cfg.Auth.APIURL, cfg.Auth.APIKey, "unarr/"+Version)
		resp, err := ac.Register(ctx, agent.RegisterRequest{
			AgentID: cfg.Agent.ID,
			Name:    cfg.Agent.Name,
			Version: Version,
		})
		if err != nil {
			accountCh <- accountResult{err: err}
			return
		}
		accountCh <- accountResult{user: resp.User}
	}()

	cyan.Println("  Account")
	ar := <-accountCh
	if ar.err != nil {
		dim.Println("    Could not fetch account info")
	} else {
		fmt.Printf("    User:       %s\n", ar.user.Name)
		fmt.Printf("    Email:      %s\n", ar.user.Email)
		planColor := dim
		if ar.user.IsPro {
			planColor = green
		}
		planColor.Printf("    Plan:       %s\n", strings.ToUpper(ar.user.Plan))
	}
	fmt.Println()

	cyan.Println("  Configuration")
	agentID := cfg.Agent.ID
	if len(agentID) > 8 {
		agentID = agentID[:8] + "..."
	}
	fmt.Printf("    Agent:      %s (%s)\n", cfg.Agent.Name, agentID)
	fmt.Printf("    Server:     %s\n", cfg.Auth.APIURL)
	fmt.Printf("    Downloads:  %s\n", cfg.Download.Dir)
	fmt.Printf("    Method:     %s\n", cfg.Download.PreferredMethod)
	if cfg.Download.PreferredQuality != "" {
		fmt.Printf("    Quality:    %s\n", cfg.Download.PreferredQuality)
	}
	fmt.Printf("    Concurrent: %d\n", cfg.Download.MaxConcurrent)
	if cfg.Organize.Enabled {
		fmt.Printf("    Organize:   on")
		if cfg.Organize.MoviesDir != "" {
			fmt.Printf(" (movies: %s", cfg.Organize.MoviesDir)
			if cfg.Organize.TVShowsDir != "" {
				fmt.Printf(", tv: %s", cfg.Organize.TVShowsDir)
			}
			fmt.Print(")")
		}
		fmt.Println()
	}
	fmt.Println()

	// ── Disk ──
	if cfg.Download.Dir != "" {
		if free, total, err := agent.DiskInfo(cfg.Download.Dir); err == nil && total > 0 {
			usedPct := float64(total-free) / float64(total) * 100
			cyan.Println("  Disk")
			fmt.Printf("    Free: %s / %s (%.0f%% used)\n", formatBytes(free), formatBytes(total), usedPct)
			if dirSize, err := agent.DirSize(cfg.Download.Dir); err == nil {
				fmt.Printf("    Downloads:  %s\n", formatBytes(dirSize))
			}
			if usedPct > 90 {
				yellow.Println("    ⚠  Low disk space!")
			}
			fmt.Println()
		}
	}

	// ── Daemon ──
	cyan.Println("  Daemon")
	state := agent.ReadState()
	if state != nil && isDaemonAlive(state) {
		green.Printf("    Status:     running (PID %d)\n", state.PID)
		fmt.Printf("    Uptime:     %s\n", formatDuration(time.Since(state.StartedAt)))
		fmt.Printf("    Last beat:  %s ago\n", formatDuration(time.Since(state.LastHeartbeat)))
		fmt.Printf("    Active:     %d task(s)\n", state.ActiveTasks)
		fmt.Printf("    Completed:  %d\n", state.CompletedCount)
		if state.FailedCount > 0 {
			fmt.Printf("    Failed:     %d\n", state.FailedCount)
		}
		if state.TotalDownloaded > 0 {
			fmt.Printf("    Downloaded: %s\n", formatBytes(state.TotalDownloaded))
		}
		if len(state.MethodStats) > 0 {
			parts := make([]string, 0, len(state.MethodStats))
			for method, count := range state.MethodStats {
				parts = append(parts, fmt.Sprintf("%s:%d", method, count))
			}
			fmt.Printf("    Methods:    %s\n", strings.Join(parts, ", "))
		}
	} else {
		dim.Println("    Status:     stopped")
		dim.Println("    Start with: unarr start")
	}
	fmt.Println()

	// ── Update check (cached: instant if <1h, otherwise async 3s) ──
	type versionResult struct {
		version   string
		fromCache bool
		err       error
	}
	versionCh := make(chan versionResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		v, cached, err := upgrade.CheckLatestCached(ctx)
		versionCh <- versionResult{v, cached, err}
	}()

	cyan.Println("  Update")
	fmt.Print("    Checking... ")
	vr := <-versionCh
	if vr.err != nil {
		dim.Println("could not check (offline?)")
	} else {
		currentClean := strings.TrimPrefix(Version, "v")
		if currentClean == vr.version {
			green.Printf("✓ up to date (v%s)\n", vr.version)
		} else {
			yellow.Printf("v%s available! ", vr.version)
			fmt.Printf("Run: unarr upgrade\n")
		}
	}
	fmt.Println()

	return nil
}

// isDaemonAlive checks if the daemon process from the state file is still running.
// Guards against PID reuse by also checking heartbeat recency.
func isDaemonAlive(state *agent.DaemonState) bool {
	if state.PID == 0 {
		return false
	}
	// Reject stale state: if last heartbeat is older than 2 minutes, the daemon
	// likely crashed and the PID may have been reused by another process.
	if !state.LastHeartbeat.IsZero() && time.Since(state.LastHeartbeat) > 2*time.Minute {
		return false
	}
	return agent.IsProcessAlive(state.PID)
}

// formatFeatures returns a comma-separated list of available features, or "".
func formatFeatures(f agent.FeatureFlags) string {
	var features []string
	if f.Torrent {
		features = append(features, "Torrent")
	}
	if f.Debrid {
		features = append(features, "Debrid")
	}
	if f.Usenet {
		features = append(features, "Usenet")
	}
	return strings.Join(features, ", ")
}

// formatBytes formats bytes into human-readable string.
func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// formatDuration formats a duration into a compact human-readable string.
func formatDuration(d time.Duration) string {
	if d < 0 {
		return "0s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	return fmt.Sprintf("%dd %dh", days, hours)
}

package cmd

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

func newDoctorCmd() *cobra.Command {
	var fix, dryRun, yes bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose configuration and connectivity",
		Long: `Run diagnostic checks to verify that unarr is correctly configured.

Checks performed:
  - Config file exists and is readable
  - API key is configured
  - API server is reachable (with latency)
  - Discovery API (search/stats) is reachable
  - Agent is registered with the server
  - Download directory exists and is writable
  - Disk space is sufficient (warns below 10 GB)
  - Current unarr version

Pass --fix to auto-repair the common misconfigurations: malformed api_url,
empty mirror list, missing download directory, config file permissions,
an unregistered agent (when an API key exists), and a corrupt config.toml
(always asks first, even with --yes). --fix backs up config.toml before
writing anything; use --dry-run to preview.

Use this command to troubleshoot connection issues or verify setup.`,
		Example: `  unarr doctor
  unarr doctor --fix
  unarr doctor --fix --dry-run`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(doctorOpts{fix: fix, dryRun: dryRun, yes: yes})
		},
	}
	cmd.Flags().BoolVar(&fix, "fix", false, "auto-repair common misconfigurations (safe, reversible)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "with --fix, show what would change without writing")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "with --fix, skip confirmation prompts")
	return cmd
}

type doctorOpts struct {
	fix    bool
	dryRun bool
	yes    bool
}

func runDoctor(opts doctorOpts) error {
	bold := color.New(color.Bold)
	green := color.New(color.FgGreen)
	red := color.New(color.FgRed)
	yellow := color.New(color.FgYellow)

	fmt.Println()
	bold.Println("  unarr Diagnostics")
	fmt.Println()

	pass := 0
	fail := 0
	warn := 0

	check := func(name string, fn func() (string, error)) {
		msg, err := fn()
		if err != nil {
			red.Printf("  x %s", name)
			if msg != "" {
				fmt.Printf(" — %s", msg)
			}
			fmt.Println()
			fail++
		} else if msg != "" && msg[0] == '!' {
			yellow.Printf("  ! %s", name)
			fmt.Printf(" — %s", msg[1:])
			fmt.Println()
			warn++
		} else {
			green.Printf("  + %s", name)
			if msg != "" {
				fmt.Printf(" — %s", msg)
			}
			fmt.Println()
			pass++
		}
	}

	// Config
	bold.Println("  Config")
	cfg := loadConfig()

	check("Config file", func() (string, error) {
		path := resolvedConfigPath()
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return path + " (not found, run unarr init)", fmt.Errorf("missing")
		}
		if configFileBroken(path) {
			return path + " (unreadable or invalid TOML — run `unarr doctor --fix`)", fmt.Errorf("corrupt")
		}
		return path, nil
	})

	check("API key configured", func() (string, error) {
		key := effectiveAPIKey(&cfg)
		if key == "" {
			return "run unarr init to configure", fmt.Errorf("missing")
		}
		if len(key) > 8 {
			return key[:8] + "...", nil
		}
		return "set", nil
	})

	fmt.Println()
	bold.Println("  Connectivity")

	// API connectivity. getClient routes through the discovery pool (see
	// discoveryHosts), so label the host actually probed — printing the raw
	// api_url here would blame unarr.app for a torrentclaw.to failure (and
	// vice versa). The configured api_url itself is exercised by the "Agent
	// registration" check below.
	discoveryBase, _ := discoveryHosts(cfg.Auth.APIURL, cfg.Auth.Mirrors)
	check("API reachable", func() (string, error) {
		client := getClient()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		start := time.Now()
		_, err := client.Health(ctx)
		elapsed := time.Since(start)
		if err != nil {
			return discoveryBase, err
		}
		return fmt.Sprintf("%s (%dms)", discoveryBase, elapsed.Milliseconds()), nil
	})

	// Discovery API reachability. The plain "API reachable" check above hits
	// /api/health, which the unarr.app brand deployment serves (200) even though
	// it brand-blocks search/stats (404). Probing an actual discovery endpoint is
	// the only way doctor catches a primary that answers health but 404s the
	// catalog — the exact failure `unarr search`/`stats` would hit.
	check("Discovery API (search/stats)", func() (string, error) {
		client := getClient()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := client.Stats(ctx); err != nil {
			return "run `unarr doctor --fix` (routes discovery to a working mirror)", err
		}
		return "reachable", nil
	})

	// Agent registration. Doubles as the API-key validity probe: a 401/403
	// from Register means the key itself was rejected (classifyAuthError maps
	// it to the `unarr login` remedy), distinct from "no key" above.
	check("Agent registration", func() (string, error) {
		key := effectiveAPIKey(&cfg)
		if key == "" {
			return "no API key", fmt.Errorf("skipped")
		}
		if cfg.Agent.ID == "" {
			return "no agent ID — run `unarr doctor --fix` (or unarr init)", fmt.Errorf("not registered")
		}

		ac := agent.NewClient(cfg.Auth.APIURL, key, "unarr/"+Version)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resp, err := ac.Register(ctx, agent.RegisterRequest{
			AgentID: cfg.Agent.ID,
			Name:    cfg.Agent.Name,
			OS:      runtime.GOOS,
			Arch:    runtime.GOARCH,
			Version: Version,
		})
		if err != nil {
			return "", classifyAuthError(err)
		}
		return fmt.Sprintf("%s (%s) [%s]", resp.User.Name, resp.User.Email, resp.User.Plan), nil
	})

	fmt.Println()
	bold.Println("  Downloads")

	check("Download directory", func() (string, error) {
		dir := cfg.Download.Dir
		if dir == "" {
			return "not configured, run unarr init", fmt.Errorf("missing")
		}
		fi, err := os.Stat(dir)
		if os.IsNotExist(err) {
			return dir + " (does not exist)", fmt.Errorf("missing")
		}
		if !fi.IsDir() {
			return dir + " (not a directory)", fmt.Errorf("invalid")
		}
		return dir, nil
	})

	check("Download dir writable", func() (string, error) {
		dir := cfg.Download.Dir
		if dir == "" {
			return "", fmt.Errorf("not configured")
		}
		tmpFile := dir + "/.unarr_write_test"
		f, err := os.Create(tmpFile)
		if err != nil {
			return "", fmt.Errorf("not writable: %w", err)
		}
		f.Close()
		os.Remove(tmpFile)
		return "OK", nil
	})

	check("Disk space", func() (string, error) {
		dir := cfg.Download.Dir
		if dir == "" {
			return "", fmt.Errorf("not configured")
		}
		return checkDiskSpace(dir)
	})

	fmt.Println()
	bold.Println("  Version")

	check("unarr version", func() (string, error) {
		return fmt.Sprintf("%s (%s/%s)", Version, runtime.GOOS, runtime.GOARCH), nil
	})

	// Summary
	fmt.Println()
	if fail == 0 && warn == 0 {
		green.Println("  All checks passed!")
	} else if fail == 0 {
		yellow.Printf("  %d passed, %d warnings\n", pass, warn)
	} else {
		red.Printf("  %d passed, %d failed, %d warnings\n", pass, fail, warn)
	}
	fmt.Println()

	if opts.fix {
		return runDoctorRepairs(&cfg, opts)
	}
	if fail > 0 {
		dim := color.New(color.Faint)
		dim.Println("  Tip: run `unarr doctor --fix` to auto-repair common issues.")
		fmt.Println()
	}

	return nil
}

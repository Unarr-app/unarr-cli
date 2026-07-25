package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/usenet/postprocess"
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
			return path + " (not found — " + setupHint(cfg.Auth.APIURL) + ")", fmt.Errorf("missing")
		}
		if configFileBroken(path) {
			return path + " (unreadable or invalid TOML — run `unarr doctor --fix`)", fmt.Errorf("corrupt")
		}
		return path, nil
	})

	check("API key configured", func() (string, error) {
		key := effectiveAPIKey(&cfg)
		if key == "" {
			return setupHint(cfg.Auth.APIURL) + " to configure it", fmt.Errorf("missing")
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
			return "no agent ID — run `unarr doctor --fix` (or " + setupHint(cfg.Auth.APIURL) + ")", fmt.Errorf("not registered")
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
			return "not configured — set UNARR_DOWNLOAD_DIR or " + setupHint(cfg.Auth.APIURL), fmt.Errorf("missing")
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

	// par2 is only exercised by the usenet method — a missing binary there
	// means deliveries can't be verified or repaired (they ship UNVERIFIED),
	// so surface it as a warning, not a failure.
	check("par2 (usenet verify/repair)", func() (string, error) {
		return par2CheckResult(cfg)
	})

	// Managed-VPN P2P kill-switch: when [downloads.vpn] required=true, torrent must
	// have a live tunnel — otherwise it's disabled (safe) and this flags it.
	check("Managed VPN (P2P kill-switch)", func() (string, error) {
		return checkManagedVPN(cfg)
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

// par2CheckResult reports whether par2 is available when the usenet method is
// enabled. Uses the canonical MethodOrder() resolver so it honors the list vs
// legacy-singular precedence, "auto", casing and whitespace — a hand-rolled
// scan would false-"not needed" on preferred_methods=["Usenet"] and hide that
// usenet ships UNVERIFIED. Returns a "!"-prefixed warning message (not an
// error) when the binary is missing — a missing par2 degrades verification but
// isn't a hard failure.
func par2CheckResult(cfg config.Config) (string, error) {
	usenetEnabled := false
	for _, m := range cfg.Download.MethodOrder() {
		if m == "usenet" {
			usenetEnabled = true
			break
		}
	}
	if !usenetEnabled {
		return "not needed (usenet not in preferred_methods)", nil
	}
	if postprocess.Par2Available() {
		return "installed", nil
	}
	return "!not installed — usenet downloads are delivered UNVERIFIED (install: apt install par2 / brew install par2)", nil
}

// errVPNKillSwitch marks the doctor "Managed VPN" check as FAILED. The message the
// user sees comes from the returned string; the error's only job is to make
// runDoctor's check() render a red ✗.
var errVPNKillSwitch = errors.New("vpn kill-switch requirement not met")

// vpnDoctorInput is the pure input to the managed-VPN doctor decision: config
// knobs + the live daemon state. Grouped in a struct to stay within the
// argument-limit and to keep evaluateVPNDoctor unit-testable.
type vpnDoctorInput struct {
	required      bool // [downloads.vpn] required
	enabled       bool // [downloads.vpn] enabled (managed)
	hasConfigFile bool // [downloads.vpn] config_file set (self-hosted)
	daemonAlive   bool
	vpnActive     bool
	vpnBlocking   bool
}

// evaluateVPNDoctor is the pure decision for the doctor "Managed VPN" check. It
// returns a (message, error) pair in the check() convention: non-nil error → FAIL,
// a leading '!' in the message → WARN, otherwise PASS. No I/O, so it's testable.
func evaluateVPNDoctor(in vpnDoctorInput) (string, error) {
	if !in.required {
		if in.enabled || in.hasConfigFile {
			return "configured (kill-switch off — a failed tunnel falls back to clear-net)", nil
		}
		return "off (not required)", nil
	}
	if !in.enabled && !in.hasConfigFile {
		return "required=true but the VPN is OFF — torrent/P2P disabled. Run `unarr vpn enable`, set a config_file, or set [downloads.vpn] required=false", errVPNKillSwitch
	}
	if !in.daemonAlive {
		return "!required — daemon not running; start it (`unarr start`) to bring the tunnel up (torrent stays disabled until it is healthy)", nil
	}
	if in.vpnBlocking {
		return "required but tunnel DOWN — P2P disabled (safe, no leak). Check `unarr daemon logs` for [vpn]; verify the add-on at https://unarr.app/vpn", errVPNKillSwitch
	}
	if in.vpnActive {
		return "required and tunnel ACTIVE — P2P protected", nil
	}
	return "!required — tunnel not yet active; the agent is bringing it up", nil
}

// checkManagedVPN reads the live daemon state and runs the pure decision. It is the
// thin I/O wrapper the doctor check() closure calls.
func checkManagedVPN(cfg config.Config) (string, error) {
	state := agent.ReadState()
	in := vpnDoctorInput{
		required:      cfg.Download.VPN.Required,
		enabled:       cfg.Download.VPN.Enabled,
		hasConfigFile: cfg.Download.VPN.ConfigFile != "",
		daemonAlive:   state != nil && isDaemonAlive(state),
	}
	if state != nil {
		in.vpnActive = state.VPNActive
		in.vpnBlocking = state.VPNBlocking
	}
	return evaluateVPNDoctor(in)
}

package cmd

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/doctor"
)

// remedyDoctorFix is the remedy for every check that `unarr doctor --fix`
// actually repairs (see planRepairs). It is metadata for machine consumers
// only — the console already prints the same hint once, at the end.
const remedyDoctorFix = "run `unarr doctor --fix`"

// hasRepairableFailure reports whether any FAILING check is one that
// `unarr doctor --fix` actually repairs, which is what decides whether the
// trailing tip is worth printing.
//
// It reads the same Remedy field the JSON exposes, so the console hint and the
// machine-readable advice can never disagree — the alternative, a second list
// of repairable check names, would be one more thing to forget to update when
// planRepairs grows a case.
func hasRepairableFailure(rep doctor.Report) bool {
	for _, c := range rep.Checks {
		if c.Status == doctor.StatusFail && c.Remedy == remedyDoctorFix {
			return true
		}
	}
	return false
}

// doctorSpecs assembles the ordered check list. The bodies stay in this package
// because they lean on its config/client helpers; internal/doctor only knows
// how to run and render them. Order is the display order, and grouping drives
// the console section headers — do not reorder without checking both.
func doctorSpecs(cfg *config.Config) []doctor.Spec {
	specs := doctorConfigSpecs(cfg)
	specs = append(specs, doctorConnectivitySpecs(cfg)...)
	specs = append(specs, doctorDownloadSpecs(cfg)...)
	specs = append(specs, doctorMediaSpecs(cfg)...)
	specs = append(specs, doctorDaemonSpec())
	return append(specs, doctor.Spec{
		Group: "Version",
		Name:  "unarr version",
		Quick: true,
		Fn: func() (string, error) {
			return fmt.Sprintf("%s (%s/%s)", Version, runtime.GOOS, runtime.GOARCH), nil
		},
	})
}

func doctorConfigSpecs(cfg *config.Config) []doctor.Spec {
	return []doctor.Spec{
		{
			Group:  "Config",
			Name:   "Config file",
			Quick:  true,
			Remedy: remedyDoctorFix,
			Fn: func() (string, error) {
				path := resolvedConfigPath()
				if _, err := os.Stat(path); os.IsNotExist(err) {
					return path + " (not found — " + setupHint(cfg.Auth.APIURL) + ")", fmt.Errorf("missing")
				}
				if configFileBroken(path) {
					return path + " (unreadable or invalid TOML — run `unarr doctor --fix`)", fmt.Errorf("corrupt")
				}
				return path, nil
			},
		},
		// Unknown keys are inert — TOML decoding drops them silently, so the setting
		// the user believes they wrote never takes effect. WARN, never FAIL.
		{
			Group: "Config",
			Name:  "Config keys",
			Quick: true,
			Fn:    func() (string, error) { return configKeysCheckResult(*cfg) },
		},
		{
			Group: "Config",
			Name:  "Config values",
			Quick: true,
			Fn:    func() (string, error) { return configValuesCheckResult(*cfg) },
		},
		{
			Group:  "Config",
			Name:   "API key configured",
			Remedy: setupHint(cfg.Auth.APIURL) + " to configure it",
			Fn: func() (string, error) {
				key := effectiveAPIKey(cfg)
				if key == "" {
					return setupHint(cfg.Auth.APIURL) + " to configure it", fmt.Errorf("missing")
				}
				if len(key) > 8 {
					return key[:8] + "...", nil
				}
				return "set", nil
			},
		},
	}
}

func doctorConnectivitySpecs(cfg *config.Config) []doctor.Spec {
	// getClient routes through the discovery pool (see discoveryHosts), so label
	// the host actually probed — printing the raw api_url here would blame
	// unarr.app for a torrentclaw.to failure (and vice versa). The configured
	// api_url itself is exercised by the "Agent registration" check below.
	discoveryBase, _ := discoveryHosts(cfg.Auth.APIURL, cfg.Auth.Mirrors)

	return []doctor.Spec{
		{
			Group: "Connectivity",
			Name:  "API reachable",
			Fn: func() (string, error) {
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
			},
		},
		// The plain "API reachable" check above hits /api/health, which the
		// unarr.app brand deployment serves (200) even though it brand-blocks
		// search/stats (404). Probing an actual discovery endpoint is the only way
		// doctor catches a primary that answers health but 404s the catalog — the
		// exact failure `unarr search`/`stats` would hit.
		{
			Group:  "Connectivity",
			Name:   "Discovery API (search/stats)",
			Remedy: remedyDoctorFix,
			Fn: func() (string, error) {
				client := getClient()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if _, err := client.Stats(ctx); err != nil {
					return "run `unarr doctor --fix` (routes discovery to a working mirror)", err
				}
				return "reachable", nil
			},
		},
		// Doubles as the API-key validity probe: a 401/403 from Register means the
		// key itself was rejected (classifyAuthError maps it to the `unarr login`
		// remedy), distinct from "no key" above.
		{
			Group:  "Connectivity",
			Name:   "Agent registration",
			Remedy: remedyDoctorFix,
			Fn:     func() (string, error) { return doctorAgentRegistration(cfg) },
		},
	}
}

func doctorAgentRegistration(cfg *config.Config) (string, error) {
	key := effectiveAPIKey(cfg)
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
	// NO NAME, NO EMAIL. This message is not only printed on the user's own
	// terminal: `unarr doctor --json` feeds the web health panel, and
	// `unarr support-bundle` embeds the whole report in a file the user attaches
	// to a public issue. Printing the account holder's name and email address
	// put both into every bundle ever generated.
	//
	// Nothing is lost by dropping them. The bundle now publishes the agent ID
	// (see internal/support/redact_config.go), which resolves to the same
	// account server-side and is what support actually looks the user up by —
	// the email was a second, weaker copy of an identifier we already ship.
	// The plan stays because it is diagnostic: a feature failing on a free
	// account is not a bug.
	return fmt.Sprintf("registered [%s]", resp.User.Plan), nil
}

func doctorDownloadSpecs(cfg *config.Config) []doctor.Spec {
	return []doctor.Spec{
		{
			Group:  "Downloads",
			Name:   "Download directory",
			Quick:  true,
			Remedy: remedyDoctorFix,
			Fn: func() (string, error) {
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
			},
		},
		{
			Group: "Downloads",
			Name:  "Download dir writable",
			Quick: true,
			// NOT remedyDoctorFix. `--fix` creates a MISSING download dir
			// (planRepairs 3 and 4) and chmods the config FILE, but nothing in
			// it touches the mode of a directory that exists and is not
			// writable — which is every way this check can fail. Pointing at
			// `doctor --fix` here would send the user to a command that
			// reports nothing to repair.
			Remedy: "grant write permission on the directory (or set a different download_dir)",
			Fn: func() (string, error) {
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
			},
		},
		{
			Group: "Downloads",
			Name:  "Disk space",
			Quick: true,
			Fn: func() (string, error) {
				dir := cfg.Download.Dir
				if dir == "" {
					return "", fmt.Errorf("not configured")
				}
				return checkDiskSpace(dir)
			},
		},
		// par2 is only exercised by the usenet method — a missing binary there
		// means deliveries can't be verified or repaired (they ship UNVERIFIED),
		// so surface it as a warning, not a failure.
		{
			Group:  "Downloads",
			Name:   "par2 (usenet verify/repair)",
			Remedy: "install par2 (apt install par2 / brew install par2)",
			Fn:     func() (string, error) { return par2CheckResult(*cfg) },
		},
		// Managed-VPN P2P kill-switch: when [downloads.vpn] required=true, torrent must
		// have a live tunnel — otherwise it's disabled (safe) and this flags it.
		{
			Group:  "Downloads",
			Name:   "Managed VPN (P2P kill-switch)",
			Remedy: "run `unarr vpn enable`, set [downloads.vpn] config_file, or set required=false",
			Fn:     func() (string, error) { return checkManagedVPN(*cfg) },
		},
	}
}

// doctorDaemonSpec reports whether the daemon this machine is supposed to be
// running is alive. It reads the state file and checks the PID — no network.
//
// It is the reason `--quick` exists. A container whose daemon has died keeps
// reporting "running" to Docker forever without this: the entrypoint process
// is still up, and nothing else looks at whether the thing it supervises is.
//
// A daemon that was never installed is a PASS, not a failure: `unarr` is a CLI
// too, and someone running one-off commands has no daemon by design. Only a
// registered-then-vanished daemon is a fault, and that is what a stale state
// file with a dead PID means.
func doctorDaemonSpec() doctor.Spec {
	return doctor.Spec{
		Group: "Daemon",
		Name:  "Daemon process",
		Quick: true,
		Fn: func() (string, error) {
			state := agent.ReadState()
			if state == nil {
				return "not running (no daemon installed on this machine)", nil
			}
			if isDaemonAlive(state) {
				return fmt.Sprintf("running (pid %d, up %s)", state.PID,
					time.Since(state.StartedAt).Round(time.Second)), nil
			}
			return fmt.Sprintf("state file says pid %d, but that process is gone", state.PID),
				fmt.Errorf("daemon died")
		},
	}
}

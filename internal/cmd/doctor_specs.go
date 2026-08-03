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

// doctorSpecs assembles the ordered check list. The bodies stay in this package
// because they lean on its config/client helpers; internal/doctor only knows
// how to run and render them. Order is the display order, and grouping drives
// the console section headers — do not reorder without checking both.
func doctorSpecs(cfg *config.Config) []doctor.Spec {
	specs := doctorConfigSpecs(cfg)
	specs = append(specs, doctorConnectivitySpecs(cfg)...)
	specs = append(specs, doctorDownloadSpecs(cfg)...)
	return append(specs, doctor.Spec{
		Group: "Version",
		Name:  "unarr version",
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
			Fn:    func() (string, error) { return configKeysCheckResult(*cfg) },
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
	return fmt.Sprintf("%s (%s) [%s]", resp.User.Name, resp.User.Email, resp.User.Plan), nil
}

func doctorDownloadSpecs(cfg *config.Config) []doctor.Spec {
	return []doctor.Spec{
		{
			Group:  "Downloads",
			Name:   "Download directory",
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

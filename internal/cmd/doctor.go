package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/doctor"
	"github.com/Unarr-app/unarr-cli/internal/usenet/postprocess"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

func newDoctorCmd() *cobra.Command {
	var fix, dryRun, yes, quick bool
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
  - ffmpeg / ffprobe are installed and runnable (the whole streaming path
    depends on them), with the libx264 + aac encoders, zscale for HDR
    tonemapping, hardware acceleration, and the cached transcode ceiling
  - Current unarr version

Pass --fix to auto-repair the common misconfigurations: malformed api_url,
empty mirror list, missing download directory, config file permissions,
an unregistered agent (when an API key exists), and a corrupt config.toml
(always asks first, even with --yes). --fix backs up config.toml before
writing anything; use --dry-run to preview.

Pass --json to emit the whole report as one JSON object (status, per-check
group/name/status/message/remedy, and the totals) instead of the console
report — for health probes, dashboards and support bundles. --json is ignored
when combined with --fix, which is interactive.

Use this command to troubleshoot connection issues or verify setup.`,
		Example: `  unarr doctor
  unarr doctor --json
  unarr doctor --json | jq -e '.status != "fail"'
  unarr doctor --fix
  unarr doctor --fix --dry-run`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(doctorOpts{fix: fix, dryRun: dryRun, yes: yes, quick: quick})
		},
	}
	cmd.Flags().BoolVar(&fix, "fix", false, "auto-repair common misconfigurations (safe, reversible)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "with --fix, show what would change without writing")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "with --fix, skip confirmation prompts")
	cmd.Flags().BoolVar(&quick, "quick", false,
		"only the local, network-free checks, and exit 1 if any fails (for container health probes)")
	return cmd
}

type doctorOpts struct {
	fix    bool
	dryRun bool
	yes    bool
	quick  bool
}

func runDoctor(opts doctorOpts) error {
	cfg := loadConfig()
	specs := doctorSpecs(&cfg)
	// --quick drops every check that touches the network. This is a hard
	// requirement, not a speed optimisation: a HEALTHCHECK that calls the API
	// marks the container unhealthy on any transient blip, Docker restarts it,
	// and a network hiccup becomes a restart loop across the fleet.
	if opts.quick {
		specs = doctor.QuickSpecs(specs)
	}

	// --json is machine output, so it must be the only thing on stdout: no
	// banner, no summary, no tip. --fix wins over it, because the repair flow
	// prompts and narrates — interleaving that with a JSON document would
	// produce something neither a human nor `jq` can read. So `--json --fix`
	// stays human, and a caller that wants the report as data runs the two
	// invocations separately.
	if jsonOut && !opts.fix {
		report := doctor.Run(specs, nil)
		if err := doctor.RenderJSON(os.Stdout, report); err != nil {
			return err
		}
		return quickExitCode(opts, report)
	}

	// The renderer prints each check as it lands: the connectivity checks take
	// up to 10 s each, so buffering the run would look like a 30 s hang.
	r := doctor.NewTextRenderer(color.Output)
	r.Start()
	report := doctor.Run(specs, r.OnCheck)
	// Offer `--fix` only when it can actually do something. Gating on
	// "any failure at all" sent a host with no ffmpeg to a command that would
	// report nothing to repair — `--fix` installs no binaries. The decision
	// lives here rather than in the renderer because the remedy string is this
	// package's knowledge, not the renderer's.
	r.ShowFixTip = !opts.fix && hasRepairableFailure(report)
	r.Finish(report)

	if opts.fix {
		return runDoctorRepairs(&cfg, opts)
	}
	// Point at the bundle where the user is actually stuck: checks failed and
	// `--fix` was either not offered or is not going to help. Naming it here
	// rather than in the issue template alone is the difference between a
	// report that arrives with evidence and six rounds of "run this, paste
	// that". Not printed on a WARN-only run — warnings are not a support case.
	if report.Failed > 0 {
		color.New(color.Faint).Fprintln(color.Output,
			"  Reporting this? `unarr support-bundle` collects the details (redacted) into one attachable file.")
		fmt.Fprintln(color.Output)
	}
	return quickExitCode(opts, report)
}

// quickExitCode turns a --quick run into a process exit status, because that
// status is the entire output a Docker HEALTHCHECK reads.
//
// WARNINGS DO NOT MAKE A CONTAINER UNHEALTHY. A missing par2, no hwaccel, an
// unknown config key — none of those mean "restart me", and Docker's only
// response to unhealthy is to restart. Reserving the non-zero code for real
// failures is what keeps the probe from becoming a restart loop over things
// the user chose.
//
// Without --quick nothing changes: `unarr doctor` has always exited 0 whatever
// it found, and scripts depend on that.
func quickExitCode(opts doctorOpts, report doctor.Report) error {
	if opts.quick && report.Failed > 0 {
		return errQuietExit
	}
	return nil
}

// usenetInPlay reports whether usenet downloads can actually happen on this
// machine, and why not when they cannot.
//
// It exists because reading MethodOrder() alone got this WRONG on the default
// config, which is the config almost everyone runs. preferred_method = "auto"
// makes MethodOrder() return nil, and the old check read nil as "usenet is not
// in preferred_methods" and reported par2 as "not needed". Seen live on a real
// pro account: the par2 line said not needed, and the line four rows below it
// reported a healthy usenet server with ten connection slots. Usenet downloads
// were going to happen, and they were going to arrive UNVERIFIED, which is the
// one thing this check exists to prevent.
//
// When the account's flags cannot be fetched (offline, no key) it falls back to
// the config-only reading. That is today's behaviour, and a doctor run with no
// network should not start inventing new warnings.
func usenetInPlay(cfg *config.Config, features featureFn) (bool, string) {
	flags, err := features()
	if err != nil {
		for _, m := range cfg.Download.MethodOrder() {
			if m == "usenet" {
				return true, ""
			}
		}
		return false, "usenet not in preferred_methods"
	}
	if methodWanted(cfg, "usenet", flags.Usenet) {
		return true, ""
	}
	if !flags.Usenet {
		return false, "this account has no usenet add-on"
	}
	return false, "usenet not in preferred_methods"
}

// par2CheckResult reports whether par2 is available when the usenet method is
// enabled. Uses the canonical MethodOrder() resolver so it honors the list vs
// legacy-singular precedence, "auto", casing and whitespace — a hand-rolled
// scan would false-"not needed" on preferred_methods=["Usenet"] and hide that
// usenet ships UNVERIFIED. Returns a "!"-prefixed warning message (not an
// error) when the binary is missing — a missing par2 degrades verification but
// isn't a hard failure.
func par2CheckResult(cfg *config.Config, features featureFn) (string, error) {
	usenetEnabled, why := usenetInPlay(cfg, features)
	if !usenetEnabled {
		return "not needed (" + why + ")", nil
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

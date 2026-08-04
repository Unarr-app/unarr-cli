package cmd

import (
	"fmt"
	"log"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

func newConfigCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Validate config.toml and report unknown keys",
		Long: `Validate the config file without changing it.

Reports two classes of problem that would otherwise be ignored in silence:

  - unknown keys: a misspelled key, or a valid key written under the wrong
    section. TOML decoding drops those without a word, so the setting simply
    never takes effect. Each one is printed with the closest valid key.
  - out-of-range values: ports outside 0-65535, negative counts, unparseable
    durations or speeds, and unknown enum values (download method, transcode
    preset, …). Unsafe download/organize directories are reported too.

Exits non-zero when anything is reported, so it can gate a deployment. The
daemon never refuses to start over these — it only warns in its log.`,
		Example: `  unarr config check`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigCheck()
		},
	}
}

func runConfigCheck() error {
	cfg := loadConfig()
	path := resolvedConfigPath()

	issues := cfg.UnknownKeyIssues()
	issues = append(issues, cfg.ValueIssues()...)
	// Path safety lives in ValidatePaths (it needs the home dir, so it is not
	// part of the pure value checks). Reuse it rather than re-deriving the rule.
	if err := cfg.ValidatePaths(); err != nil {
		issues = append(issues, config.Issue{Key: "paths", Message: err.Error()})
	}

	fmt.Println()
	color.New(color.Bold).Printf("  unarr config check — %s\n", path)
	fmt.Println()

	if len(issues) == 0 {
		color.New(color.FgGreen).Println("  + no problems found")
		fmt.Println()
		return nil
	}

	yellow := color.New(color.FgYellow)
	for _, iss := range issues {
		yellow.Printf("  ! %s\n", iss)
	}
	fmt.Println()
	return fmt.Errorf("%d configuration problem(s) found in %s", len(issues), path)
}

// unknownKeyWarnings renders the loaded config's unrecognised keys as warning
// lines. Shared so the daemon log and `unarr doctor` say exactly the same thing.
func unknownKeyWarnings(cfg config.Config) []string {
	issues := cfg.UnknownKeyIssues()
	out := make([]string, 0, len(issues))
	for _, iss := range issues {
		out = append(out, iss.String())
	}
	return out
}

// logUnknownConfigKeys writes one log line per unrecognised key at daemon
// start-up, so a typo surfaces in `unarr daemon logs` instead of being ignored.
// Never fatal: a stale key must not keep an agent from running.
func logUnknownConfigKeys(cfg config.Config) {
	for _, w := range unknownKeyWarnings(cfg) {
		log.Printf("[config] %s", w)
	}
}

// configKeysCheckResult is the doctor check for unrecognised config keys.
// Returns a "!"-prefixed WARN and never an error: an unknown key is inert, and
// failing here would paint doctor red for everyone the day a key is renamed.
func configKeysCheckResult(cfg config.Config) (string, error) {
	warnings := unknownKeyWarnings(cfg)
	if len(warnings) == 0 {
		return "no unknown keys", nil
	}
	return "!" + strings.Join(warnings, "; "), nil
}

// configValuesCheckResult is the doctor check for values that are syntactically
// fine but outside their accepted range — log_level = "verbose", a port of
// 99999, a negative seed_ratio.
//
// It exists because `unarr doctor` printed "All checks passed!" for a config
// `unarr config check` rejected outright: the keys check only ever looked at
// key SPELLING, so a file whose every key was correct and whose values were all
// nonsense came back clean. The two commands now agree on what is wrong; they
// still disagree, deliberately, on how loudly to say it.
//
// WARN, never FAIL, and that is the part worth defending: the loader already
// substitutes its default for a value it cannot use, so the agent is running
// correctly — just not the way the user wrote it down. Failing here would give
// --quick a non-zero exit, and Docker's answer to unhealthy is to restart, so a
// stray log_level would turn into a container restarting every 60 seconds
// forever. `unarr config check` is the one that exits non-zero, because it is
// the one a deployment gate calls on purpose.
func configValuesCheckResult(cfg config.Config) (string, error) {
	issues := cfg.ValueIssues()
	if len(issues) == 0 {
		return "no out-of-range values", nil
	}
	msgs := make([]string, 0, len(issues))
	for _, iss := range issues {
		msgs = append(msgs, iss.String())
	}
	return "!" + strings.Join(msgs, "; ") + " (run `unarr config check` for the full list)", nil
}

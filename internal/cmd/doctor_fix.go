package cmd

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/charmbracelet/huh"
	"github.com/fatih/color"
)

// normalizeAPIURL cleans a user-entered api_url. It fixes the three most common
// hand-editing mistakes that silently 404 every request:
//   - no scheme      ("torrentclaw.com"      → "https://torrentclaw.com")
//   - trailing slash ("https://x.com/"       → "https://x.com")
//   - an /api suffix ("https://x.com/api/v1" → "https://x.com")
//
// The client appends the API path itself, so a base carrying /api produces
// /api/api/v1/... → 404. Returns the cleaned URL and whether it changed. An
// empty (or whitespace-only) input is left as-is (Load defaults it). Userinfo
// (user:pass@host — self-hosters behind basic auth) and ports are preserved.
func normalizeAPIURL(raw string) (cleaned string, changed bool) {
	orig := raw
	s := strings.TrimSpace(raw)
	if s == "" {
		return orig, false
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return orig, false
	}
	// ToLower: normalize deterministically even if the stdlib ever preserves
	// scheme case ("HTTPS://x" must not be forced to https twice differently).
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		scheme = "https"
	}
	hostPart := u.Host
	if u.User != nil {
		hostPart = u.User.String() + "@" + u.Host
	}
	path := strings.TrimRight(u.Path, "/")
	if low := strings.ToLower(path); low == "/api" || strings.HasPrefix(low, "/api/") {
		path = ""
	}
	cleaned = strings.TrimRight(scheme+"://"+hostPart+path, "/")
	return cleaned, cleaned != orig
}

// repair is a single self-heal action doctor --fix can apply. Every repair in
// the current set is SAFE: offline, reversible (config is backed up first), and
// idempotent. Network/secret repairs (re-auth, ffmpeg fetch, service install)
// are surfaced as guidance by the checks, not auto-applied.
type repair struct {
	desc  string
	apply func() error
}

// planRepairs inspects cfg (and the config file at path) and returns the
// ordered list of safe repairs needed to bring it to a working baseline.
// Repairs mutate cfg in place (persisted by the caller) or touch the
// filesystem. Offline repairs come first; the one network repair (agent
// registration) is last so a network failure can't block the config fixes.
// An already-healthy config yields nil.
func planRepairs(cfg *config.Config, path string) []repair {
	var reps []repair

	// 1. Normalize a malformed api_url (scheme / trailing slash / /api suffix).
	if cleaned, changed := normalizeAPIURL(cfg.Auth.APIURL); changed {
		reps = append(reps, repair{
			desc: fmt.Sprintf("Normalize api_url: %q → %q", cfg.Auth.APIURL, cleaned),
			apply: func() error {
				cfg.Auth.APIURL = cleaned
				return nil
			},
		})
	}

	// 2. Repopulate an empty mirror list so discovery has TorrentClaw failover
	//    targets (also what makes `unarr search`/`stats` route correctly — see
	//    discoveryHosts). Uses the built-in defaults; `unarr mirrors update`
	//    refreshes them from the live brand-aware list.
	if len(cfg.Auth.Mirrors) == 0 {
		defaults := config.Default().Auth.Mirrors
		reps = append(reps, repair{
			desc: fmt.Sprintf("Set default mirror failover list: %s", strings.Join(defaults, ", ")),
			apply: func() error {
				cfg.Auth.Mirrors = defaults
				return nil
			},
		})
	}

	// 3. Configure a download directory if none is set.
	if strings.TrimSpace(cfg.Download.Dir) == "" {
		dir := defaultDownloadDir()
		reps = append(reps, repair{
			desc: fmt.Sprintf("Set download directory to default: %s", dir),
			apply: func() error {
				cfg.Download.Dir = dir
				return os.MkdirAll(dir, 0o755)
			},
		})
	} else if fi, err := os.Stat(cfg.Download.Dir); os.IsNotExist(err) {
		// 4. Create a configured-but-missing download directory.
		dir := cfg.Download.Dir
		reps = append(reps, repair{
			desc: fmt.Sprintf("Create missing download directory: %s", dir),
			apply: func() error {
				return os.MkdirAll(dir, 0o755)
			},
		})
	} else if err == nil && !fi.IsDir() {
		// Configured path is a file, not a directory — not safely auto-fixable
		// (we won't delete the user's file); left for the check to flag.
		_ = fi
	}

	// 5. Tighten config file permissions — the TOML holds the API key, so it
	//    must not be group/world readable. POSIX only: on Windows os.Chmod just
	//    toggles the read-only bit and mode bits are meaningless.
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(path); err == nil {
			if perm := fi.Mode().Perm(); perm&0o077 != 0 {
				reps = append(reps, repair{
					desc: fmt.Sprintf("Tighten config file permissions: %04o → 0600 (it holds the API key)", perm),
					apply: func() error {
						return os.Chmod(path, 0o600)
					},
				})
			}
		}
	}

	// 6. Register the agent when a key exists but no identity was persisted
	//    (interrupted init, hand-written config). Network repair — kept LAST so
	//    an unreachable server never blocks the offline config fixes.
	if r := planAgentRegistrationRepair(cfg); r != nil {
		reps = append(reps, *r)
	}

	return reps
}

// configFileBroken reports whether the config file exists but cannot be read
// or parsed as TOML. A missing file is NOT broken (Load falls back to
// defaults by design); only an existing-but-unusable file needs the
// regenerate repair.
func configFileBroken(path string) bool {
	if _, err := os.Stat(path); err != nil {
		return false
	}
	_, err := config.Load(path)
	return err != nil
}

// backupConfigFile copies the config at path to path.bak.<unix> so a --fix run
// is always reversible. Returns the backup path. A missing source is not an
// error (nothing to back up).
func backupConfigFile(path string, nowUnix int64) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	bak := fmt.Sprintf("%s.bak.%d", path, nowUnix)
	if err := os.WriteFile(bak, data, 0o600); err != nil {
		return "", err
	}
	return bak, nil
}

// confirmRepair shows a yes/no huh prompt. A huh error (usually: no TTY) is
// returned so callers can turn it into actionable guidance instead of
// silently applying changes.
func confirmRepair(title, description, affirmative string) (bool, error) {
	var apply bool
	err := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title(title).
				Description(description).
				Affirmative(affirmative).
				Negative("No, cancel").
				Value(&apply),
		),
	).Run()
	if err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return false, nil
		}
		return false, err
	}
	return apply, nil
}

// confirmRegenerateBrokenConfig is the explicit gate for the one
// destructive-ish repair: rewriting an unparseable config.toml from defaults.
// It is ALWAYS prompted — a bare --yes never consents to it — and declining
// (or having no TTY) aborts the whole repair run, because saving anything over
// a corrupt file the user did not agree to replace would destroy their data.
func consentBrokenConfigRegen(path string, dim *color.Color) (bool, error) {
	ok, err := confirmRepair(
		"config.toml is corrupt (unreadable or invalid TOML). Regenerate it from defaults?",
		fmt.Sprintf("The broken file is backed up first (%s.bak.<ts>). Settings inside it — including the API key — are NOT recovered; re-run `unarr login` afterwards if needed.", path),
		"Yes, back up and regenerate",
	)
	if err != nil {
		return false, fmt.Errorf("config.toml at %s is corrupt and regenerating it needs an interactive confirmation (%w); nothing was changed — re-run `unarr doctor --fix` in a terminal, or fix/delete the file manually", path, err)
	}
	if !ok {
		dim.Println("  Cancelled — corrupt config left untouched (nothing was written).")
		fmt.Println()
	}
	return ok, nil
}

// runDoctorRepairs applies the safe repairs planRepairs found for cfg, backing
// up config.toml first and persisting once at the end. --dry-run previews only;
// --yes skips the confirmation prompt (except the corrupt-config regenerate,
// which always asks). A repair failing does not discard the others: everything
// that succeeded is still saved and the failures are reported.
func runDoctorRepairs(cfg *config.Config, opts doctorOpts) error {
	bold := color.New(color.Bold)
	green := color.New(color.FgGreen)
	yellow := color.New(color.FgYellow)
	red := color.New(color.FgRed)
	dim := color.New(color.Faint)

	path := resolvedConfigPath()
	broken := configFileBroken(path)

	reps := planRepairs(cfg, path)
	if broken {
		// cfg is already Default()+env here (loadConfig fell back when the file
		// didn't parse); the save at the end writes the fresh TOML. The entry
		// exists so the regeneration is listed, counted, and forces a save.
		reps = append([]repair{{
			desc:  fmt.Sprintf("Regenerate corrupt config.toml from defaults (broken original backed up): %s", path),
			apply: func() error { return nil },
		}}, reps...)
	}

	fmt.Println()
	bold.Println("  Repairs")

	if len(reps) == 0 {
		green.Println("  + Nothing to repair — config looks healthy.")
		fmt.Println()
		return nil
	}

	for _, r := range reps {
		yellow.Printf("  ~ %s\n", r.desc)
	}

	if opts.dryRun {
		dim.Println("  (dry-run — no changes written)")
		fmt.Println()
		return nil
	}

	confirmed := false
	if broken {
		ok, err := consentBrokenConfigRegen(path, dim)
		if err != nil || !ok {
			return err
		}
		confirmed = true // the explicit regen consent covers the run
	}

	if !opts.yes && !confirmed {
		apply, err := confirmRepair(
			fmt.Sprintf("Apply %d repair(s)?", len(reps)),
			"config.toml is backed up first.",
			"Yes, fix it",
		)
		if err != nil {
			// Non-interactive terminal (no TTY): huh can't prompt. Guide the user
			// to the explicit opt-in rather than silently applying changes.
			return fmt.Errorf("cannot prompt for confirmation (%w); re-run with --yes to apply, or --dry-run to preview", err)
		}
		if !apply {
			dim.Println("  Cancelled.")
			fmt.Println()
			return nil
		}
	}

	bak, err := backupConfigFile(path, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("backup config: %w", err)
	}
	if bak != "" {
		dim.Printf("  backup: %s\n", bak)
	}

	applied := 0
	var failed []string
	for _, r := range reps {
		if err := r.apply(); err != nil {
			red.Printf("  x %s — %v\n", r.desc, err)
			failed = append(failed, fmt.Sprintf("%s (%v)", r.desc, err))
			continue
		}
		applied++
		green.Printf("  + %s\n", r.desc)
	}

	if applied > 0 {
		if err := config.Save(*cfg, path); err != nil {
			return fmt.Errorf("save config: %w", err)
		}
		appCfg = *cfg // keep the cached config in sync (same pattern as init/up)
		green.Printf("  + Saved %s\n", path)
	}

	if len(failed) > 0 {
		return fmt.Errorf("%d repair(s) failed (the rest were applied and saved): %s",
			len(failed), strings.Join(failed, "; "))
	}

	dim.Println("  Re-run `unarr doctor` to confirm all checks pass.")
	fmt.Println()
	return nil
}

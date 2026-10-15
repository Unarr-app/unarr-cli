package backup

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

func mustApply(t *testing.T, arc *Archive, l Layout, snapDir string) Plan {
	t.Helper()
	plan, err := arc.Plan(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(ApplyOptions{SnapshotDir: snapDir, Now: time.Unix(1_700_000_000, 0)}); err != nil {
		t.Fatal(err)
	}
	return plan
}

// skipUnlessPermsEnforced skips tests that rely on a chmod denying access:
// root bypasses it and Windows ignores POSIX modes.
func skipUnlessPermsEnforced(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("file modes do not deny access here")
	}
}

func TestRedactedRestoreKeepsExistingSecrets(t *testing.T) {
	l := newLayout(t)
	res := mustCreate(t, l, CreateOptions{})
	write(t, l.ConfigPath, strings.Replace(testConfig, `dir = "/tmp/dl"`, `dir = "/other"`, 1))
	arc, err := Open(res.Path, "")
	if err != nil {
		t.Fatal(err)
	}
	mustApply(t, arc, l, t.TempDir())
	got := read(t, l.ConfigPath)
	for _, want := range []string{`api_key = "SECRET-API-KEY"`, `agent_hash = "SECRET-AGENT-HASH"`,
		`webdav_username = "SECRET-DAV-USER"`, `webdav_password = 'SECRET-DAV'`} {
		if !strings.Contains(got, want) {
			t.Errorf("existing secret wiped, want %s in:\n%s", want, got)
		}
	}
	if !strings.Contains(got, `dir = "/tmp/dl"`) {
		t.Fatalf("non-secret setting not restored:\n%s", got)
	}
}

// The api_key and agent_hash a redacted restore keeps were minted for THIS
// machine's agent id: restoring the archive's id next to them would pair one
// machine's credentials with another's identity. The local id stays, and the
// plan says so.
func TestRedactedRestoreKeepsLocalAgentID(t *testing.T) {
	l := newLayout(t)
	res := mustCreate(t, l, CreateOptions{})
	write(t, l.ConfigPath, strings.Replace(testConfig, `id = "agent-1"`, `id = "agent-local"`, 1))
	arc, err := Open(res.Path, "")
	if err != nil {
		t.Fatal(err)
	}
	plan := mustApply(t, arc, l, t.TempDir())
	w := strings.Join(plan.Warnings, "\n")
	if !strings.Contains(w, `"agent-local"`) || !strings.Contains(w, `"agent-1"`) || !strings.Contains(w, "unarr login") {
		t.Fatalf("plan does not say the local agent id was kept: %v", plan.Warnings)
	}
	got := read(t, l.ConfigPath)
	if !strings.Contains(got, `id = "agent-local"`) || strings.Contains(got, "agent-1") {
		t.Fatalf("archive's agent id paired with the local credentials:\n%s", got)
	}
	if !strings.Contains(got, `api_key = "SECRET-API-KEY"`) || !strings.Contains(got, `agent_hash = "SECRET-AGENT-HASH"`) {
		t.Fatalf("local credentials not kept:\n%s", got)
	}
}

// Without local credentials there is nothing the id is bound to: the archive's
// id is restored (the replacement-machine path, `unarr login` follows) — and so
// it is from an encrypted archive, which carries its own matching credentials.
func TestRestoreUsesArchiveAgentIDWhenNotBoundLocally(t *testing.T) {
	cases := map[string]struct {
		opts    CreateOptions
		current string
	}{
		"redacted, no local credentials": {CreateOptions{}, "[agent]\nid = \"agent-local\"\n"},
		"encrypted archive":              {CreateOptions{Passphrase: "pw"}, strings.Replace(testConfig, `id = "agent-1"`, `id = "agent-local"`, 1)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			l := newLayout(t)
			res := mustCreate(t, l, tc.opts)
			write(t, l.ConfigPath, tc.current)
			arc, err := Open(res.Path, tc.opts.Passphrase)
			if err != nil {
				t.Fatal(err)
			}
			plan := mustApply(t, arc, l, t.TempDir())
			if w := strings.Join(plan.Warnings, "\n"); strings.Contains(w, "agent id") {
				t.Fatalf("unexpected agent id warning: %v", plan.Warnings)
			}
			if got := read(t, l.ConfigPath); !strings.Contains(got, `id = "agent-1"`) {
				t.Fatalf("archive's agent id not restored:\n%s", got)
			}
		})
	}
}

// An archive whose config has no [agent] id line (hand-written, or made before
// the machine signed in) must not drop the local id either: it is added back.
func TestRedactedRestoreAddsLocalAgentIDTheArchiveLacks(t *testing.T) {
	for name, archived := range map[string]string{
		"no [agent] table": "[auth]\napi_key = \"OLD-KEY\"\n[downloads]\ndir = \"/tmp/dl\"\n",
		"no id key":        "[auth]\napi_key = \"OLD-KEY\"\n[agent]\nname = \"box\"\n[downloads]\ndir = \"/tmp/dl\"\n",
		"[agent] at EOF":   "[auth]\napi_key = \"OLD-KEY\"\n[downloads]\ndir = \"/tmp/dl\"\n[agent]",
	} {
		t.Run(name, func(t *testing.T) {
			l := newLayout(t)
			write(t, l.ConfigPath, archived)
			res := mustCreate(t, l, CreateOptions{})
			write(t, l.ConfigPath, testConfig)
			arc, _ := Open(res.Path, "")
			mustApply(t, arc, l, t.TempDir())
			cfg, err := config.Load(l.ConfigPath)
			if err != nil || cfg.Agent.ID != "agent-1" || cfg.Auth.APIKey != "SECRET-API-KEY" || cfg.Download.Dir != "/tmp/dl" {
				t.Fatalf("restored config: %v %+v %+v\n%s", err, cfg.Agent, cfg.Auth, read(t, l.ConfigPath))
			}
		})
	}
}

// A credential the line merge cannot place (here: a dotted key) must be named in
// the plan, not silently dropped.
func TestPlanWarnsWhenSecretCannotBeCarried(t *testing.T) {
	l := newLayout(t)
	res := mustCreate(t, l, CreateOptions{})
	write(t, l.ConfigPath, "auth.api_key = \"SECRET-DOTTED\"\n")
	arc, _ := Open(res.Path, "")
	plan, err := arc.Plan(l)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan.Warnings, "\n"), "auth.api_key") {
		t.Fatalf("no warning about auth.api_key: %v", plan.Warnings)
	}
}

// On a machine with no config.toml there are no credentials to keep: the plan
// must say to sign in rather than promise to keep them.
func TestPlanOnFreshMachineSaysToSignIn(t *testing.T) {
	l := newLayout(t)
	res := mustCreate(t, l, CreateOptions{})
	arc, _ := Open(res.Path, "")
	fresh := Layout{ConfigPath: filepath.Join(t.TempDir(), "config.toml"), DataDir: t.TempDir()}
	plan, err := arc.Plan(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if w := strings.Join(plan.Warnings, "\n"); !strings.Contains(w, "unarr login") || strings.Contains(w, "are kept") {
		t.Fatalf("warnings: %v", plan.Warnings)
	}
}

// The plan only promises to keep this machine's credentials when there are some:
// a config.toml without any gets the `unarr login` hint instead, and so does
// one whose credentials cannot be carried over.
func TestPlanOnlyPromisesCredentialsThatExist(t *testing.T) {
	l := newLayout(t)
	res := mustCreate(t, l, CreateOptions{})
	arc, _ := Open(res.Path, "")
	for current, kept := range map[string]bool{
		"[agent]\nid = \"agent-local\"\n[downloads]\ndir = \"/x\"\n": false,
		"auth.api_key = \"SECRET-DOTTED\"\n":                         false,
		"[auth]\napi_key = \"SECRET-LOCAL\"\n":                       true,
	} {
		write(t, l.ConfigPath, current)
		plan, err := arc.Plan(l)
		if err != nil {
			t.Fatal(err)
		}
		w := strings.Join(plan.Warnings, "\n")
		if strings.Contains(w, "are kept") != kept || strings.Contains(w, "unarr login") == kept {
			t.Errorf("current config %q: warnings: %v", current, plan.Warnings)
		}
	}
	// An archive without config.toml leaves the local one alone: nothing to say.
	if err := os.Remove(l.ConfigPath); err != nil {
		t.Fatal(err)
	}
	tasksOnly := mustCreate(t, l, CreateOptions{})
	write(t, l.ConfigPath, testConfig)
	arc, _ = Open(tasksOnly.Path, "")
	plan, err := arc.Plan(l)
	if w := strings.Join(plan.Warnings, "\n"); err != nil || strings.Contains(w, "config") || strings.Contains(w, "unarr login") {
		t.Fatalf("tasks-only archive: %v %v", err, plan.Warnings)
	}
}

func TestPlanWritesNothing(t *testing.T) {
	l := newLayout(t)
	res := mustCreate(t, l, CreateOptions{})
	write(t, l.ConfigPath, "changed = 1\n")
	arc, _ := Open(res.Path, "")
	plan, err := arc.Plan(l)
	if err != nil || plan.Applied || !plan.Changes() {
		t.Fatalf("plan: %v %+v", err, plan)
	}
	if read(t, l.ConfigPath) != "changed = 1\n" {
		t.Fatal("Plan modified the config")
	}
	if len(plan.Items) != 2 || plan.Items[0].Action != ActionOverwrite {
		t.Fatalf("plan: %+v", plan.Items)
	}
}

func TestApplyTakesPreRestoreSnapshot(t *testing.T) {
	l := newLayout(t)
	res := mustCreate(t, l, CreateOptions{})
	write(t, l.ConfigPath, "current = true\n")
	arc, _ := Open(res.Path, "")
	plan := mustApply(t, arc, l, t.TempDir())
	if !strings.HasPrefix(filepath.Base(plan.PreRestore), KindPreRestore.prefix()) {
		t.Fatalf("snapshot: %q", plan.PreRestore)
	}
	prev, err := Open(plan.PreRestore, "")
	if err != nil || string(prev.files[nameConfig]) != "current = true\n" {
		t.Fatalf("snapshot does not hold the previous state: %v", err)
	}
	if prev.Manifest.Secrets != SecretsIncluded {
		t.Fatalf("snapshot must be a verbatim copy, got secrets=%s", prev.Manifest.Secrets)
	}
}

// When the second file cannot be written the first must be put back: a
// restore lands whole or not at all.
func TestApplyRollsBackOnWriteFailure(t *testing.T) {
	skipUnlessPermsEnforced(t)
	l := newLayout(t)
	res := mustCreate(t, l, CreateOptions{Passphrase: "pw"})
	write(t, l.ConfigPath, "current = true\n")
	write(t, filepath.Join(l.DataDir, nameTasks), "[]")
	arc, err := Open(res.Path, "pw")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := arc.Plan(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(l.DataDir, 0o500); err != nil { // the tasks write now fails
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(l.DataDir, 0o755) })
	err = plan.Apply(ApplyOptions{SnapshotDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "put back") {
		t.Fatalf("err = %v, want a rolled-back failure", err)
	}
	if got := read(t, l.ConfigPath); got != "current = true\n" {
		t.Fatalf("config.toml not rolled back:\n%s", got)
	}
	if plan.Applied {
		t.Fatal("plan reported as applied")
	}
}

func TestPlanFailsOnUnreadableCurrentFile(t *testing.T) {
	skipUnlessPermsEnforced(t)
	l := newLayout(t)
	res := mustCreate(t, l, CreateOptions{})
	if err := os.Chmod(l.ConfigPath, 0); err != nil {
		t.Fatal(err)
	}
	arc, _ := Open(res.Path, "")
	if _, err := arc.Plan(l); err == nil || !strings.Contains(err.Error(), "read current") {
		t.Fatalf("err = %v: an unreadable config.toml must not be planned as a fresh create", err)
	}
}

// A dotfiles-managed config.toml is a symlink: the restore must update the file
// it points to and leave the link in place.
func TestRestoreWritesThroughSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	l := newLayout(t)
	res := mustCreate(t, l, CreateOptions{Passphrase: "pw"})
	real := filepath.Join(t.TempDir(), "dotfiles", "unarr.toml")
	write(t, real, "current = true\n")
	if err := os.Remove(l.ConfigPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, l.ConfigPath); err != nil {
		t.Fatal(err)
	}
	arc, _ := Open(res.Path, "pw")
	mustApply(t, arc, l, t.TempDir())
	if st, err := os.Lstat(l.ConfigPath); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink replaced by a regular file: %v %v", st.Mode(), err)
	}
	if read(t, real) != testConfig {
		t.Fatal("symlink target not restored")
	}
}

func TestApplyWithNothingChangedTakesNoSnapshot(t *testing.T) {
	l := newLayout(t)
	res := mustCreate(t, l, CreateOptions{Passphrase: "pw"})
	arc, _ := Open(res.Path, "pw")
	snap := t.TempDir()
	plan := mustApply(t, arc, l, snap)
	if plan.Changes() || plan.Applied || plan.PreRestore != "" {
		t.Fatalf("plan: %+v", plan)
	}
	if es, _ := os.ReadDir(snap); len(es) != 0 {
		t.Fatal("snapshot written for a no-op restore")
	}
}

package backup

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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

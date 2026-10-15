package cmd

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/flock"

	"github.com/Unarr-app/unarr-cli/internal/backup"
	"github.com/Unarr-app/unarr-cli/internal/config"
)

// backupEnv points the CLI at a throw-away HOME-like tree and resets the
// package-level config cache so loadConfig reads it.
func backupEnv(t *testing.T, extra string) (cfgPath string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("UNARR_CONFIG_DIR", filepath.Join(root, "cfg"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "data"))
	t.Setenv(backupPassphraseEnv, "")
	oldFile, oldLoaded := cfgFile, cfgLoaded
	cfgFile, cfgLoaded = "", false
	t.Cleanup(func() { cfgFile, cfgLoaded = oldFile, oldLoaded })

	cfgPath = config.FilePath()
	writeBackupTestFile(t, cfgPath, "[auth]\napi_key = \"KEY-123\"\n[downloads]\ndir = \"/tmp/dl\"\n"+extra)
	return cfgPath
}

func writeBackupTestFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runBackup(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cfgLoaded = false // re-read config.toml, as a fresh process would
	c := newBackupCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs(args)
	err := c.Execute()
	return out.String(), err
}

func localBackupDir() string { return filepath.Join(config.Dir(), "backups") }

func TestBackupCLICreateListRestoreDryRun(t *testing.T) {
	cfgPath := backupEnv(t, "")

	out, err := runBackup(t, "create")
	if err != nil || !strings.Contains(out, "Backup written") || !strings.Contains(out, "NOT included") {
		t.Fatalf("create: %v\n%s", err, out)
	}
	es, _ := backup.List(localBackupDir())
	if len(es) != 1 {
		t.Fatalf("expected 1 backup in the default dir, got %d", len(es))
	}
	if out, err = runBackup(t, "list"); err != nil || !strings.Contains(out, es[0].Name) {
		t.Fatalf("list: %v\n%s", err, out)
	}

	writeBackupTestFile(t, cfgPath, "[downloads]\ndir = \"/changed\"\n")
	out, err = runBackup(t, "restore", es[0].Path)
	if err != nil || !strings.Contains(out, "Dry-run") || !strings.Contains(out, "overwrite") {
		t.Fatalf("dry-run: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(cfgPath); !strings.Contains(string(b), "/changed") {
		t.Fatal("dry-run modified config.toml")
	}

	if out, err = runBackup(t, "restore", es[0].Path, "--apply"); err != nil || !strings.Contains(out, "Previous state saved to") {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(cfgPath); !strings.Contains(string(b), "/tmp/dl") {
		t.Fatalf("config not restored:\n%s", b)
	}
}

func TestBackupCLIRestoreBlockedByRunningDaemon(t *testing.T) {
	backupEnv(t, "")
	if _, err := runBackup(t, "create"); err != nil {
		t.Fatal(err)
	}
	es, _ := backup.List(localBackupDir())

	lock := flock.New(config.LockPath())
	if ok, err := lock.TryLock(); err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	defer lock.Unlock()

	_, err := runBackup(t, "restore", es[0].Path, "--apply")
	if !errors.Is(err, errDaemonRunning) {
		t.Fatalf("err = %v, want errDaemonRunning", err)
	}
	// A dry-run is harmless and still allowed.
	if _, err := runBackup(t, "restore", es[0].Path); err != nil {
		t.Fatalf("dry-run with daemon: %v", err)
	}
}

// The lock is HELD while the restore writes, so a daemon cannot start between
// the check and the writes (a check that released it first could not promise it).
func TestRestoreHoldsInstanceLockWhileApplying(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "cfg", "unarr.lock")
	err := withInstanceLock(lockPath, func() error {
		if ok, err := flock.New(lockPath).TryLock(); err != nil || ok {
			t.Errorf("a daemon could take the lock mid-restore: ok=%v err=%v", ok, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	other := flock.New(lockPath)
	if ok, err := other.TryLock(); err != nil || !ok {
		t.Fatalf("lock not released after the restore: ok=%v err=%v", ok, err)
	}
	other.Unlock()
}

// The pre-restore snapshot holds the CURRENT secrets in the clear: it must
// stay beside config.toml even when [backup] dir is a NAS or cloud folder.
func TestPreRestoreSnapshotStaysLocal(t *testing.T) {
	nas := t.TempDir()
	cfgPath := backupEnv(t, "[backup]\ndir = "+tomlQuote(nas)+"\n")
	pass := filepath.Join(t.TempDir(), "pass")
	writeBackupTestFile(t, pass, "pw\n")
	if _, err := runBackup(t, "create", "--encrypt", "--passphrase-file", pass); err != nil {
		t.Fatal(err)
	}
	es, _ := backup.List(nas)
	if len(es) != 1 {
		t.Fatalf("archive not in [backup] dir: %v", es)
	}
	body, _ := os.ReadFile(cfgPath)
	writeBackupTestFile(t, cfgPath, strings.Replace(string(body), "KEY-123", "KEY-ROTATED", 1))

	out, err := runBackup(t, "restore", es[0].Path, "--apply", "--passphrase-file", pass)
	if err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	for _, e := range mustList(t, nas) {
		if e.Kind == backup.KindPreRestore {
			t.Fatalf("pre-restore snapshot (cleartext secrets) written to the [backup] dir: %s", e.Path)
		}
	}
	local := mustList(t, localBackupDir())
	if len(local) != 1 || local[0].Kind != backup.KindPreRestore {
		t.Fatalf("snapshot not kept beside config.toml: %+v", local)
	}
}

func mustList(t *testing.T, dir string) []backup.Entry {
	t.Helper()
	es, err := backup.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	return es
}

func tomlQuote(s string) string { return "'" + s + "'" }

// --encrypt with an empty passphrase must fail, never fall back to a plain zip.
func TestBackupCreateRejectsEmptyPassphraseFile(t *testing.T) {
	backupEnv(t, "")
	empty := filepath.Join(t.TempDir(), "empty")
	writeBackupTestFile(t, empty, "\n")
	out, err := runBackup(t, "create", "--encrypt", "--passphrase-file", empty)
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if es, _ := backup.List(localBackupDir()); len(es) != 0 {
		t.Fatalf("an archive was written: %v", es)
	}
}

func TestRestoreOnFreshMachineSaysThereWasNothingToSave(t *testing.T) {
	cfgPath := backupEnv(t, "")
	if _, err := runBackup(t, "create"); err != nil {
		t.Fatal(err)
	}
	es, _ := backup.List(localBackupDir())
	if err := os.Remove(cfgPath); err != nil {
		t.Fatal(err)
	}
	out, err := runBackup(t, "restore", es[0].Path, "--apply")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(out, "saved to \n") || !strings.Contains(out, "no previous state") {
		t.Fatalf("misleading summary:\n%s", out)
	}
}

func TestBackupCLIPrune(t *testing.T) {
	backupEnv(t, "")
	dir := localBackupDir()
	for _, n := range []string{"unarr-backup-20260101T030000Z.zip", "unarr-scheduled-20260102T030000Z.zip", "unarr-backup-20260103T030000Z.zip"} {
		writeBackupTestFile(t, filepath.Join(dir, n), "x")
	}
	if _, err := runBackup(t, "prune", "--keep", "1"); err != nil {
		t.Fatal(err)
	}
	if es, _ := backup.List(dir); len(es) != 1 || !strings.Contains(es[0].Name, "20260103") {
		t.Fatalf("after prune: %+v", es)
	}
}

// A relative [backup] dir is invalid — `unarr config check` flags it and the
// daemon will not schedule it — so the subcommands refuse it too, before
// touching anything, instead of resolving it against the working directory.
func TestBackupCommandsRefuseRelativeConfigDir(t *testing.T) {
	backupEnv(t, "[backup]\ndir = \"rel\"\n")
	cwd := t.TempDir()
	t.Chdir(cwd)
	stray := filepath.Join(cwd, "rel")
	for _, n := range []string{"unarr-backup-20260101T030000Z.zip", "unarr-backup-20260102T030000Z.zip"} {
		writeBackupTestFile(t, filepath.Join(stray, n), "x")
	}
	for _, args := range [][]string{{"prune", "--keep", "1"}, {"list"}, {"create"}} {
		out, err := runBackup(t, args...)
		if err == nil || !strings.Contains(err.Error(), "backup.dir") || !strings.Contains(err.Error(), "absolute") {
			t.Fatalf("%v: err = %v\n%s", args, err, out)
		}
	}
	if es := mustList(t, stray); len(es) != 2 {
		t.Fatalf("a relative [backup] dir was used: %+v", es)
	}
	// Commands that do not use [backup] dir are not blocked by it.
	if out, err := runBackup(t, "create", "--out", filepath.Join(t.TempDir(), "b.zip")); err != nil {
		t.Fatalf("create --out: %v\n%s", err, out)
	}
	if out, err := runBackup(t, "list", "--dir", stray); err != nil || !strings.Contains(out, "20260102") {
		t.Fatalf("list --dir: %v\n%s", err, out)
	}
}

// An invalid [backup] table is reported once at daemon start instead of being
// silently never scheduled.
func TestDaemonLogsInvalidBackupSchedule(t *testing.T) {
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })
	cfg := config.Default()
	cfg.Backup = config.BackupConfig{Enabled: true, Time: "3am"}
	startBackupScheduler(context.Background(), cfg)
	if !strings.Contains(logs.String(), "scheduled backups disabled") || !strings.Contains(logs.String(), "backup.time") {
		t.Fatalf("no startup warning: %q", logs.String())
	}
}

func TestConfigCheckFlagsBadBackupTable(t *testing.T) {
	cfg := loadTestConfig(t, "[backup]\nenabled = true\nfrequency = \"hourly\"\ntime = \"3am\"\n")
	msg, err := configValuesCheckResult(cfg)
	if err != nil || !strings.Contains(msg, "backup.frequency") || !strings.Contains(msg, "backup.time") {
		t.Fatalf("config check did not flag [backup]: %q %v", msg, err)
	}
	if k, _ := configKeysCheckResult(loadTestConfig(t, "[backup]\nenabled = true\nretention = 3\n")); !strings.Contains(k, "backup.retention") {
		t.Fatalf("unknown [backup] key not reported: %q", k)
	}
}

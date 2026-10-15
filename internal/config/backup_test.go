package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupDefaults(t *testing.T) {
	var b BackupConfig
	h, m, err := b.ClockTime()
	if err != nil || h != 3 || m != 0 {
		t.Fatalf("default time = %d:%d %v", h, m, err)
	}
	if b.Weekly() || b.Retention() != 3 || b.WeekdayOrDefault() != 0 {
		t.Fatalf("defaults: weekly=%v %d %v", b.Weekly(), b.Retention(), b.WeekdayOrDefault())
	}
	if want := filepath.Join(Dir(), "backups"); b.Directory("") != want {
		t.Fatalf("dir = %s, want %s", b.Directory(""), want)
	}
	if b.Validate() != nil {
		t.Fatalf("zero table invalid: %v", b.Validate())
	}
}

// The default backup dir follows the config file actually in use, so an agent
// run with --config never shares (and prunes) another agent's archives.
func TestBackupDirFollowsConfigPath(t *testing.T) {
	other := filepath.Join(t.TempDir(), "dev", "config.toml")
	var b BackupConfig
	if got, want := b.Directory(other), filepath.Join(filepath.Dir(other), "backups"); got != want {
		t.Fatalf("dir = %s, want %s", got, want)
	}
	// Absolute on every platform: "/srv/b" has no drive letter, so Windows does
	// not count it as absolute.
	explicit := filepath.Join(t.TempDir(), "b")
	b.Dir = explicit
	if got := b.Directory(other); got != explicit {
		t.Fatalf("explicit dir ignored: %s", got)
	}
}

func TestBackupValueIssues(t *testing.T) {
	ok := Default()
	ok.Backup = BackupConfig{Enabled: true, Frequency: "Weekly", Time: "23:59", Weekday: "Friday", RetentionCount: 7, Dir: filepath.Join(t.TempDir(), "b")}
	if iss := ok.Backup.valueIssues(); len(iss) != 0 {
		t.Fatalf("valid config flagged: %v", iss)
	}
	if !ok.Backup.Weekly() {
		t.Fatal(`"Weekly" not recognised as weekly`)
	}
	bad := BackupConfig{Frequency: "hourly", Time: "3am", Weekday: "funday", RetentionCount: -1, Dir: "rel/dir"}
	got := bad.valueIssues()
	for _, key := range []string{"backup.frequency", "backup.time", "backup.weekday", "backup.retention_count", "backup.dir"} {
		found := false
		for _, i := range got {
			found = found || i.Key == key
		}
		if !found {
			t.Errorf("no issue for %s in %v", key, got)
		}
	}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "backup.frequency") {
		t.Errorf("Validate = %v, want the first issue", err)
	}
	// Reaches `unarr config check` through ValueIssues.
	c := Default()
	c.Backup = bad
	n := 0
	for _, i := range c.ValueIssues() {
		if strings.HasPrefix(i.Key, "backup.") {
			n++
		}
	}
	if n != 5 {
		t.Fatalf("ValueIssues surfaced %d backup issues, want 5", n)
	}
}

// CheckedDirectory is the gate for code about to read, write or delete in the
// backup dir: it refuses exactly what Validate (and `unarr config check`)
// rejects, so the CLI, the daemon and config check cannot disagree.
func TestBackupCheckedDirectoryMatchesValidate(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	abs := t.TempDir()
	for dir, bad := range map[string]bool{
		"": false, abs: false, " " + abs + " ": false,
		"rel": true, "./backups": true, "~/backups": true,
	} {
		b := BackupConfig{Dir: dir}
		got, err := b.CheckedDirectory(cfgPath)
		if (err != nil) != bad || (b.Validate() != nil) != bad {
			t.Errorf("dir %q: CheckedDirectory err = %v, Validate = %v, want refused=%v", dir, err, b.Validate(), bad)
			continue
		}
		if bad {
			if !strings.Contains(err.Error(), "backup.dir") || !strings.Contains(err.Error(), "absolute") {
				t.Errorf("dir %q: unclear error %q", dir, err)
			}
		} else if got != b.Directory(cfgPath) {
			t.Errorf("dir %q: CheckedDirectory = %q, Directory = %q", dir, got, b.Directory(cfgPath))
		}
	}
}

func TestBackupTableLoadsAndIsOmittedWhenUnset(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte("[backup]\nenabled = true\nfrequency = \"weekly\"\ntime = \"04:30\"\nretention_count = 5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if b := cfg.Backup; !b.Enabled || b.Retention() != 5 || len(cfg.UnknownKeys()) != 0 {
		t.Fatalf("loaded %+v unknown=%v", b, cfg.UnknownKeys())
	}

	// A config that never mentions [backup] must not grow one on Save.
	q := filepath.Join(t.TempDir(), "config.toml")
	if err := Save(Default(), q); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(q)
	if strings.Contains(string(b), "[backup]") {
		t.Fatalf("Save added a [backup] table to an untouched config:\n%s", b)
	}
}

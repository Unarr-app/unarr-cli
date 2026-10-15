package backup

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// zipMember is member name of the plain archive at path, read without Open: a
// pre-restore snapshot may hold a file over the cap Open enforces.
func zipMember(t *testing.T, path, name string) []byte {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		_, err = b.ReadFrom(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	t.Fatalf("%s has no %s", path, name)
	return nil
}

// A local config.toml over its 4 MiB cap — 40 MiB of NUL bytes is what an older
// unarr restored from a zip bomb — must not lock out the restore that replaces
// it: the snapshot takes it as is, and a good backup restores over it.
func TestRestoreOverOversizedLocalConfigTakesSnapshot(t *testing.T) {
	for name, local := range map[string]string{
		"40 MiB of NUL bytes":       strings.Repeat("\x00", 40<<20),
		"valid TOML, one byte over": paddedConfig(maxConfigSize + 1),
	} {
		for _, pass := range []string{"pw", ""} {
			t.Run(name+"/passphrase="+pass, func(t *testing.T) {
				l := newLayout(t)
				res := mustCreate(t, l, CreateOptions{Passphrase: pass})
				write(t, l.ConfigPath, local)
				arc, err := Open(res.Path, pass)
				if err != nil {
					t.Fatal(err)
				}
				plan := mustApply(t, arc, l, t.TempDir())
				if plan.PreRestore == "" {
					t.Fatal("no pre-restore snapshot taken")
				}
				if !bytes.Equal(zipMember(t, plan.PreRestore, nameConfig), []byte(local)) {
					t.Fatal("the snapshot does not hold the config it replaced")
				}
				if got := read(t, l.ConfigPath); !strings.Contains(got, `dir = "/tmp/dl"`) || len(got) > maxConfigSize {
					t.Fatalf("the backup's config was not restored (%d bytes)", len(got))
				}
			})
		}
	}
}

// Past even the shared bound the snapshot cannot be taken: nothing changes, and
// the error says how to go on — move the oversized file aside.
func TestSnapshotOverSharedBoundSaysWhatToDo(t *testing.T) {
	withContentLimit(t, 64<<10)
	l := newLayout(t)
	local := strings.Repeat("\x00", 100<<10)
	write(t, l.ConfigPath, local)
	arc := craftArchive(t, SecretsIncluded, map[string]string{nameConfig: testConfig})
	plan, err := arc.Plan(l)
	if err != nil {
		t.Fatal(err)
	}
	snapDir := t.TempDir()
	err = plan.Apply(ApplyOptions{SnapshotDir: snapDir})
	if !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), "move "+l.ConfigPath+" aside") {
		t.Fatalf("err = %v, want ErrTooLarge saying to move %s aside", err, l.ConfigPath)
	}
	if read(t, l.ConfigPath) != local {
		t.Fatal("config changed although the snapshot failed")
	}
	if es, _ := os.ReadDir(snapDir); len(es) != 0 {
		t.Fatalf("%d file(s) written by a failed snapshot", len(es))
	}

	// Moved aside, the same restore goes through.
	if err := os.Rename(l.ConfigPath, filepath.Join(t.TempDir(), "config.toml")); err != nil {
		t.Fatal(err)
	}
	mustApply(t, arc, l, snapDir)
	if read(t, l.ConfigPath) != testConfig {
		t.Fatal("the backup's config was not restored")
	}
}

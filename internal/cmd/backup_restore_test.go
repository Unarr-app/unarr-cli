package cmd

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/backup"
)

// craftBackupFile writes a plain archive of members whose manifest matches
// them — intact, so Open accepts it whatever the members hold.
func craftBackupFile(t *testing.T, members map[string]string) string {
	t.Helper()
	m := backup.Manifest{Schema: backup.SchemaVersion, UnarrVersion: "1.0.0", OS: runtime.GOOS, Arch: runtime.GOARCH,
		CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Secrets: backup.SecretsIncluded}
	for name, body := range members {
		sum := sha256.Sum256([]byte(body))
		m.Files = append(m.Files, backup.FileEntry{Name: name, Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])})
	}
	mj, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "crafted.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	all := map[string]string{backup.ManifestName: string(mj)}
	for name, body := range members {
		all[name] = body
	}
	for name, body := range all {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// Open only proves the archive intact; whether unarr can load its content is
// the plan's call. So the line printed after Open says what was verified and
// never reads "Backup OK" right before the plan refuses the content.
func TestRestoreSaysWhatWasVerifiedBeforeTheContentCheck(t *testing.T) {
	backupEnv(t, "")
	p := craftBackupFile(t, map[string]string{"active-tasks.json": `[{"id":5}]`})
	for _, args := range [][]string{{"restore", p}, {"restore", p, "--apply"}} {
		out, err := runBackup(t, args...)
		if !errors.Is(err, backup.ErrBadContent) {
			t.Fatalf("%v: err = %v, want ErrBadContent\n%s", args, err, out)
		}
		if strings.Contains(out, "Backup OK") || !strings.Contains(out, "Archive verified: manifest and checksums OK") {
			t.Fatalf("%v: output claims more than Open verified:\n%s", args, out)
		}
	}
}

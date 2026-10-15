package backup

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

const testConfig = `# my comment
[auth]
api_key = "SECRET-API-KEY"
api_url = "https://x.example"

[agent]
id = "agent-1"
agent_hash = "SECRET-AGENT-HASH"

[downloads]
dir = "/tmp/dl"
webdav_username = "SECRET-DAV-USER"
webdav_password = 'SECRET-DAV'
`

const testTasks = `[{"id":"t1","infoHash":"abc","directUrl":"https://debrid/SECRET-URL","nzbPassword":"SECRET-NZB",
"sourceSet":{"version":1,"sources":[{"id":"s","directUrl":"https://d/SECRET-SRC","password":"SECRET-SRCPW","transport":"debrid"}]}}]`

var testSecrets = []string{
	"SECRET-API-KEY", "SECRET-AGENT-HASH", "SECRET-DAV-USER", "SECRET-DAV", "SECRET-URL", "SECRET-NZB",
	"SECRET-SRC", "SECRET-SRCPW", "SECRET-CTRL", "SECRET-PRIVKEY",
}

func newLayout(t *testing.T) Layout {
	t.Helper()
	root := t.TempDir()
	l := Layout{ConfigPath: filepath.Join(root, "cfg", "config.toml"), DataDir: filepath.Join(root, "data")}
	write(t, l.ConfigPath, testConfig)
	write(t, filepath.Join(l.DataDir, "active-tasks.json"), testTasks)
	// Ephemeral runtime files that must never be captured.
	write(t, filepath.Join(l.DataDir, "daemon.state.json"), `{"pid":1,"controlToken":"SECRET-CTRL"}`)
	write(t, filepath.Join(l.DataDir, "certs", "agent.key"), "SECRET-PRIVKEY")
	return l
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustCreate(t *testing.T, l Layout, o CreateOptions) Result {
	t.Helper()
	o.Layout, o.Version = l, "9.9.9"
	if o.Out == "" {
		o.Dir = filepath.Join(t.TempDir(), "bk")
	}
	res, err := Create(o)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// zipText concatenates every member of a plain archive.
func zipText(t *testing.T, path string) (string, []string) {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var all strings.Builder
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
		rc, _ := f.Open()
		var b bytes.Buffer
		_, _ = b.ReadFrom(rc)
		rc.Close()
		all.Write(b.Bytes())
	}
	return all.String(), names
}

func TestRoundTripEncrypted(t *testing.T) {
	l := newLayout(t)
	res := mustCreate(t, l, CreateOptions{Passphrase: "hunter2"})
	if !strings.HasSuffix(res.Path, ".zip.enc") {
		t.Fatalf("encrypted archive name: %s", res.Path)
	}
	if _, err := Open(res.Path, ""); !errors.Is(err, ErrPassphraseRequired) {
		t.Fatalf("no passphrase: %v", err)
	}
	if _, err := Open(res.Path, "wrong"); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	arc, err := Open(res.Path, "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	write(t, l.ConfigPath, "mutated = true\n")
	os.Remove(filepath.Join(l.DataDir, "active-tasks.json"))

	plan := mustApply(t, arc, l, t.TempDir())
	if !plan.Applied {
		t.Fatalf("not applied: %+v", plan)
	}
	if read(t, l.ConfigPath) != testConfig {
		t.Fatal("config not restored verbatim (comments/secrets lost)")
	}
	if !strings.Contains(read(t, filepath.Join(l.DataDir, "active-tasks.json")), "SECRET-URL") {
		t.Fatal("tasks not restored with secrets")
	}
}

func TestDefaultArchiveHasNoSecretsOrRuntimeFiles(t *testing.T) {
	l := newLayout(t)
	res := mustCreate(t, l, CreateOptions{})
	all, names := zipText(t, res.Path)
	for _, s := range testSecrets {
		if strings.Contains(all, s) {
			t.Errorf("default archive leaks %s", s)
		}
	}
	if len(names) != 3 {
		t.Errorf("members = %v, want manifest + config + tasks only", names)
	}
	if res.Manifest.Secrets != SecretsRedacted {
		t.Errorf("secrets mode = %s", res.Manifest.Secrets)
	}
	if st, _ := os.Stat(res.Path); st.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Errorf("backup perms %v, want 0600", st.Mode().Perm())
	}
	// The redacted config must still be valid, loadable TOML with its settings.
	cfg, err := config.Load(restoreConfigFrom(t, res.Path))
	if err != nil || cfg.Auth.APIKey != "" || cfg.Agent.Hash != "" || cfg.Download.Dir != "/tmp/dl" || cfg.Agent.ID != "agent-1" {
		t.Fatalf("redacted config: %v %+v %+v", err, cfg.Auth, cfg.Agent)
	}
}

func restoreConfigFrom(t *testing.T, file string) string {
	t.Helper()
	arc, err := Open(file, "")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "config.toml")
	write(t, p, string(arc.files[nameConfig]))
	return p
}

// Two archives in the same second (manual beside scheduled, two restores in a
// row) must both survive: a rename onto an existing name would drop one.
func TestSameSecondArchivesDoNotOverwrite(t *testing.T) {
	l := newLayout(t)
	dir := t.TempDir()
	now := time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC)
	a, err := Create(CreateOptions{Layout: l, Dir: dir, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Create(CreateOptions{Layout: l, Dir: dir, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if a.Path == b.Path {
		t.Fatalf("second archive reused %s", a.Path)
	}
	if es, _ := List(dir); len(es) != 2 {
		t.Fatalf("entries = %d, want 2", len(es))
	}
}

func TestNothingToBackup(t *testing.T) {
	l := Layout{ConfigPath: filepath.Join(t.TempDir(), "no.toml"), DataDir: t.TempDir()}
	if _, err := Create(CreateOptions{Layout: l, Dir: t.TempDir()}); !errors.Is(err, ErrNothingToBackup) {
		t.Fatalf("err = %v", err)
	}
}

func TestListAndPruneRetention(t *testing.T) {
	l := newLayout(t)
	dir := t.TempDir()
	base := time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		if _, err := Create(CreateOptions{Layout: l, Dir: dir, Now: base.AddDate(0, 0, i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := create(CreateOptions{Layout: l, Dir: dir, Now: base}, KindPreRestore); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "notes.txt"), "unrelated")
	es, _ := List(dir)
	if len(es) != 6 || es[0].CreatedAt.Before(es[1].CreatedAt) {
		t.Fatalf("list: %+v", es)
	}
	removed, err := Prune(dir, 3)
	if err != nil || len(removed) != 2 {
		t.Fatalf("prune: %v %v", removed, err)
	}
	es, _ = List(dir)
	if len(es) != 4 { // 3 regular + 1 pre-restore
		t.Fatalf("after prune: %d entries", len(es))
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Fatal("prune touched an unrelated file")
	}
	if es[0].CreatedAt != base.AddDate(0, 0, 4) {
		t.Fatalf("newest was pruned: %v", es[0].CreatedAt)
	}
	if n, _ := Prune(dir, 0); len(n) != 2 { // keep<1 clamps to 1: never delete the last copy
		t.Fatalf("clamp: removed %v", n)
	}
}

// Pre-restore retention: the newest PreRestoreKeep stay, anything younger than
// PreRestoreMaxAge stays, the spared snapshot stays, and other kinds are not
// its business.
func TestPrunePreRestoreRetention(t *testing.T) {
	l := newLayout(t)
	dir := t.TempDir()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	snap := func(age time.Duration) string {
		t.Helper()
		res, err := create(CreateOptions{Layout: l, Dir: dir, Now: now.Add(-age)}, KindPreRestore)
		if err != nil {
			t.Fatal(err)
		}
		return res.Path
	}
	day := 24 * time.Hour
	var young []string
	for _, age := range []time.Duration{time.Hour, 2 * day, 3 * day, 4 * day} {
		young = append(young, snap(age))
	}
	expired := snap(PreRestoreMaxAge + day)
	spared := snap(PreRestoreMaxAge + 2*day)
	manual, err := Create(CreateOptions{Layout: l, Dir: dir, Now: now.Add(-365 * day)})
	if err != nil {
		t.Fatal(err)
	}

	removed, err := PrunePreRestore(dir, now, spared)
	if err != nil || len(removed) != 1 || removed[0] != expired {
		t.Fatalf("removed %v (%v), want only %s", removed, err, expired)
	}
	for _, p := range append(young, spared, manual.Path) {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s removed: %v", filepath.Base(p), err)
		}
	}
	// Past the age limit, the newest PreRestoreKeep still survive.
	later := now.Add(10 * PreRestoreMaxAge)
	if _, err := PrunePreRestore(dir, later, ""); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range mustListDir(t, dir) {
		if e.Kind == KindPreRestore {
			n++
		}
	}
	if n != PreRestoreKeep {
		t.Fatalf("%d snapshots left, want %d", n, PreRestoreKeep)
	}
}

func mustListDir(t *testing.T, dir string) []Entry {
	t.Helper()
	es, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	return es
}

// craft builds a zip with arbitrary members.
func craft(t *testing.T, members map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for n, c := range members {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(c))
	}
	zw.Close()
	p := filepath.Join(t.TempDir(), "x.zip")
	write(t, p, buf.String())
	return p
}

func manifestFor(name, body string) string {
	return `{"schema":1,"unarrVersion":"1","os":"linux","arch":"amd64","createdAt":"2026-01-01T00:00:00Z","secrets":"redacted","files":[{"name":` +
		`"` + name + `","size":` + strconv.Itoa(len(body)) + `,"sha256":"` + sumHex([]byte(body)) + `"}]}`
}

func TestZipSlipRejected(t *testing.T) {
	for _, name := range []string{"../evil.txt", "..\\evil.txt", "/etc/passwd", "sub/config.toml", "C:evil"} {
		p := craft(t, map[string]string{ManifestName: manifestFor(name, "x=1"), name: "x=1"})
		if _, err := Open(p, ""); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("%q: err = %v, want ErrUnsafePath", name, err)
		}
	}
}

func TestUnknownMemberNotRestored(t *testing.T) {
	l := newLayout(t)
	p := craft(t, map[string]string{ManifestName: manifestFor("evil.sh", "x=1"), "evil.sh": "x=1"})
	arc, err := Open(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := arc.Plan(l); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v, want ErrUnsafePath (allowlist)", err)
	}
}

func TestCorruptChecksumRejected(t *testing.T) {
	body := "a = 1\n"
	p := craft(t, map[string]string{ManifestName: manifestFor(nameConfig, body), nameConfig: "a = 2\n"})
	if _, err := Open(p, ""); !errors.Is(err, ErrChecksum) {
		t.Fatalf("err = %v, want ErrChecksum", err)
	}
	// An extra member the manifest does not list is rejected too.
	p = craft(t, map[string]string{ManifestName: manifestFor(nameConfig, body), nameConfig: body, nameTasks: "[]"})
	if _, err := Open(p, ""); !errors.Is(err, ErrBadManifest) {
		t.Fatalf("extra member: %v", err)
	}
}

func TestTamperedEncryptedArchiveRejected(t *testing.T) {
	l := newLayout(t)
	res := mustCreate(t, l, CreateOptions{Passphrase: "pw"})
	b, _ := os.ReadFile(res.Path)
	for _, i := range []int{len(encMagic) + 1, len(b) - 3} { // a salt byte (header is GCM AAD) and a ciphertext byte
		c := append([]byte{}, b...)
		c[i] ^= 0xff
		write(t, res.Path, string(c))
		if _, err := Open(res.Path, "pw"); !errors.Is(err, ErrBadPassphrase) {
			t.Fatalf("byte %d flipped: err = %v", i, err)
		}
	}
}

func TestNewerSchemaRejected(t *testing.T) {
	m := strings.Replace(manifestFor(nameConfig, "a"), `"schema":1`, `"schema":99`, 1)
	p := craft(t, map[string]string{ManifestName: m, nameConfig: "a"})
	if _, err := Open(p, ""); !errors.Is(err, ErrBadManifest) {
		t.Fatalf("err = %v", err)
	}
}

// A stray multi-GB file handed to restore must be refused before it is read
// into memory; a sparse file makes the size real without writing the bytes.
func TestOpenRejectsOversizedArchive(t *testing.T) {
	p := filepath.Join(t.TempDir(), "huge.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxArchiveSize + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := Open(p, ""); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	members := map[string]string{ManifestName: manifestFor(nameConfig, "a")}
	for i := 0; i < maxMembers; i++ {
		members["m"+strconv.Itoa(i)] = "x"
	}
	if _, err := Open(craft(t, members), ""); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("member count: err = %v, want ErrTooLarge", err)
	}
}

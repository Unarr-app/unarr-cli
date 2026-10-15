package backup

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// manifestOf lists members with their sizes and SHA-256, the way Create would.
func manifestOf(t *testing.T, secrets string, members map[string]string) string {
	t.Helper()
	m := Manifest{Schema: SchemaVersion, UnarrVersion: "1", OS: runtime.GOOS, Arch: runtime.GOARCH, Secrets: secrets}
	for name, body := range members {
		m.Files = append(m.Files, FileEntry{Name: name, Size: int64(len(body)), SHA256: sumHex([]byte(body))})
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// craftArchive is a well-formed archive of members, checksums and all, so only
// their content is in question.
func craftArchive(t *testing.T, secrets string, members map[string]string) *Archive {
	t.Helper()
	all := map[string]string{ManifestName: manifestOf(t, secrets, members)}
	for n, b := range members {
		all[n] = b
	}
	arc, err := Open(craft(t, all), "")
	if err != nil {
		t.Fatalf("crafted archive does not open: %v", err)
	}
	return arc
}

// snapshotState records the files a restore onto l may touch.
func snapshotState(t *testing.T, l Layout) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, s := range l.sources() {
		out[s.path] = read(t, s.path)
	}
	return out
}

// A restore refuses, before writing anything, a member unarr could not load:
// the plan — the dry-run, and what --apply runs first — fails, so neither the
// files nor a pre-restore snapshot are ever written.
func TestRestoreRefusesContentUnarrCannotLoad(t *testing.T) {
	cases := map[string]struct{ member, body string }{
		"config of NUL bytes":       {nameConfig, strings.Repeat("\x00", 1<<10)},
		"config not TOML":           {nameConfig, "[auth\napi_key = \"k\"\n"},
		"config of the wrong types": {nameConfig, "[downloads]\ndir = 5\n"},
		"queue not JSON":            {nameTasks, `[{"id":"t1"`},
		"queue not a list":          {nameTasks, `{"id":"t1"}`},
		"queue not of objects":      {nameTasks, `[1, 2]`},
		// JSON lists of objects the daemon's store still drops whole.
		"queue with an id that is a number": {nameTasks, `[{"id":5}]`},
		"queue with a queuedAt not a time":  {nameTasks, `[{"id":"t","queuedAt":"not-a-time"}]`},
	}
	for name, c := range cases {
		for _, secrets := range []string{SecretsRedacted, SecretsIncluded} {
			t.Run(name+"/"+secrets, func(t *testing.T) {
				l := newLayout(t)
				before := snapshotState(t, l)
				members := map[string]string{nameConfig: testConfig, nameTasks: testTasks}
				members[c.member] = c.body
				_, err := craftArchive(t, secrets, members).Plan(l)
				if !errors.Is(err, ErrBadContent) || !strings.Contains(err.Error(), c.member) {
					t.Fatalf("err = %v, want ErrBadContent naming %s", err, c.member)
				}
				for p, want := range before {
					if read(t, p) != want {
						t.Fatalf("%s changed by a refused restore", p)
					}
				}
			})
		}
	}
}

// checkTasks accepts exactly what the daemon's store loads — json.Unmarshal of
// the whole file into []agent.Task — although it decodes one task at a time.
// Refusing less lets through a queue the daemon drops whole; refusing more
// blocks a restore the daemon would have taken. The seeds run with go test.
func FuzzCheckTasksAgreesWithDaemonLoad(f *testing.F) {
	for _, q := range []string{
		testTasks, `[]`, ` [ ] `, `null`, `[null]`, `[{"id":"t"}]  `,
		`[{"id":"t","queuedAt":"2026-01-01T00:00:00Z","extra":{"deep":[1,{"a":null}]}}]`,
		`[{"id":5}]`, `[{"id":"t","queuedAt":"not-a-time"}]`, `[{"id":"t","contentId":1.5}]`,
		`[{"id":"t","sourceSet":{"sources":5}}]`, `[1]`, `[[]]`, `{}`, `"x"`, `5`, `true`,
		``, `   `, `[`, `[1,]`, `[{"id":"a"} {"id":"b"}]`, `[] []`, `[] x`, `[]]`, "\xEF\xBB\xBF[]",
	} {
		f.Add([]byte(q))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var tasks []agent.Task
		daemon := json.Unmarshal(data, &tasks)
		check := checkTasks(data)
		if (daemon == nil) != (check == nil) {
			t.Fatalf("queue %q: the daemon's load says %v, checkTasks says %v", data, daemon, check)
		}
		if check != nil && !errors.Is(check, ErrBadContent) {
			t.Fatalf("queue %q: err = %v, want ErrBadContent", data, check)
		}
	})
}

// A secret-free config is checked as it would be written: merged with this
// machine's credentials, exactly as they are written here.
func TestRestoreChecksMergedConfig(t *testing.T) {
	l := newLayout(t)
	// \q is no TOML escape: this config.toml does not load, and its api_key
	// would carry that into the restored one.
	write(t, l.ConfigPath, "[auth]\napi_key = \"BAD\\q\"\n")
	arc := craftArchive(t, SecretsRedacted, map[string]string{nameConfig: "[auth]\napi_key = \"\"\n"})
	_, err := arc.Plan(l)
	if !errors.Is(err, ErrBadContent) || !strings.Contains(err.Error(), "merged") {
		t.Fatalf("err = %v, want ErrBadContent for the merged config", err)
	}
	if got := read(t, l.ConfigPath); got != "[auth]\napi_key = \"BAD\\q\"\n" {
		t.Fatalf("config changed by a refused restore:\n%s", got)
	}
	// The way out the README gives: with config.toml moved aside, it restores.
	if err := os.Rename(l.ConfigPath, l.ConfigPath+".aside"); err != nil {
		t.Fatal(err)
	}
	mustApply(t, arc, l, t.TempDir())
	if got := read(t, l.ConfigPath); got != "[auth]\napi_key = \"\"\n" {
		t.Fatalf("restored config:\n%s", got)
	}
}

// Create refuses what restore would refuse, before writing anything, encrypted
// or not: an archive that cannot be restored is the worst a backup can be. A
// config that does not parse is reported as such, not as a redaction failure
// that --encrypt would get around.
func TestCreateRefusesContentRestoreWouldRefuse(t *testing.T) {
	for name, c := range map[string]struct{ member, body string }{
		"config not TOML": {nameConfig, "not toml at all = = =\n"},
		"queue not JSON":  {nameTasks, `[{"id":"t1"`},
	} {
		for _, pass := range []string{"", "pw"} {
			l := newLayout(t)
			p := l.ConfigPath
			if c.member == nameTasks {
				p = filepath.Join(l.DataDir, nameTasks)
			}
			write(t, p, c.body)
			dir := t.TempDir()
			_, err := Create(CreateOptions{Layout: l, Dir: dir, Passphrase: pass})
			if !errors.Is(err, ErrBadContent) || !strings.Contains(err.Error(), c.member) {
				t.Fatalf("%s (passphrase %q): err = %v, want ErrBadContent naming %s", name, pass, err, c.member)
			}
			if es, _ := os.ReadDir(dir); len(es) != 0 {
				t.Fatalf("%s (passphrase %q): refused, yet %d file(s) written", name, pass, len(es))
			}
		}
	}
}

// The pre-restore snapshot is not checked: a good backup restores over a
// config that does not parse — the very case restore is for — and the broken
// one is kept, verbatim, in the snapshot.
func TestRestoreOverUnloadableConfigKeepsItInSnapshot(t *testing.T) {
	l := newLayout(t)
	res := mustCreate(t, l, CreateOptions{Passphrase: "pw"})
	const broken = "[auth\nthis is not = = toml\n"
	write(t, l.ConfigPath, broken)
	arc, err := Open(res.Path, "pw")
	if err != nil {
		t.Fatal(err)
	}
	plan := mustApply(t, arc, l, t.TempDir())
	if read(t, l.ConfigPath) != testConfig {
		t.Fatal("the backup's config was not restored")
	}
	snap, err := Open(plan.PreRestore, "")
	if err != nil {
		t.Fatalf("pre-restore snapshot does not open: %v", err)
	}
	if string(snap.files[nameConfig]) != broken {
		t.Fatal("the snapshot does not hold the config it replaced")
	}
}

package support

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/doctor"
)

// The two markers the leak tests hunt for.
//
// They are DIFFERENT strings on purpose. sentinelSecret goes into the fields
// classified Secret, so the Scrubber holds its value and can erase it from free
// text; sentinelPublic goes into every other string field, where the Scrubber
// does NOT hold it. If the two were the same marker, a bug that copied a
// publishable path straight into the bundle would be silently cleaned up by the
// literal scrubber and the test would stay green while leaking.
const (
	sentinelSecret = "SECRET-SENTINEL-DO-NOT-LEAK"
	sentinelPublic = "PUBLIC-FIELD-SENTINEL-DO-NOT-LEAK"
	// The one value the bundle is SUPPOSED to carry out. Separate from the two
	// above so the leak assertions stay exhaustive over everything else.
	sentinelAgentID = "AGENT-ID-SENTINEL-MUST-TRAVEL"
)

// TestBundleLeaksNoSentinel is the empirical proof of the redaction.
//
// It fills a fully-populated Config with markers, plants the same markers in
// every other place the bundle reads from — the daemon log, the error log, the
// state file, the active-task file, the bench cache and the doctor report —
// and asserts that not one of them survives into the generated bundle, section
// bodies and manifest alike.
//
// The markers are planted in the shapes a credential really takes in a log
// line (bare, api_key=…, ?t=…, Bearer …) because those are the shapes the
// scrubber has to cope with. A bare marker in a log line is caught only because
// it is also the value of a Secret config field — which is exactly the
// mechanism, and the reason the literal pass exists at all.
func TestBundleLeaksNoSentinel(t *testing.T) {
	dir := withDataDir(t)
	cfg := populatedConfig()

	writeFile(t, filepath.Join(dir, "unarr.log"), leakyLogLines())
	writeFile(t, filepath.Join(dir, "unarr.err.log"), leakyLogLines())
	writeFile(t, filepath.Join(dir, "unarr.boot.log"), leakyLogLines())
	writeFile(t, filepath.Join(dir, "daemon.state.json"), leakyState())
	writeFile(t, filepath.Join(dir, "active-tasks.json"), leakyTasks())
	writeFile(t, filepath.Join(dir, "bench-encode.json"), `{"cliVersion":"`+sentinelSecret+`"}`)

	b := Collect(Inputs{
		Config:         cfg,
		Version:        "9.9.9",
		Doctor:         func() (doctor.Report, error) { return leakyDoctorReport(), nil },
		Logs:           testLogPaths(dir),
		BenchCachePath: filepath.Join(dir, "bench-encode.json"),
	})

	manifest, err := b.Manifest()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	whole := string(manifest)
	for _, s := range b.Sections {
		whole += "\n" + s.Name + "\n" + string(s.body)
	}
	for _, marker := range []string{sentinelSecret, sentinelPublic} {
		if n := strings.Count(whole, marker); n != 0 {
			t.Errorf("marker %q leaked %d time(s) into the bundle:\n%s", marker, n, whole)
		}
	}
	// The mirror image, and it belongs in this test rather than a separate one:
	// the cheapest way to make the loop above pass is to publish nothing, and
	// this is the assertion that says which single value must survive it.
	if !strings.Contains(whole, sentinelAgentID) {
		t.Errorf("the agent ID did not reach the bundle — support cannot look the user up without it")
	}
}

// TestBundleIsNotVacuouslyClean guards the test above. A bundle that collected
// nothing would also leak nothing; these are the sections that must be present
// and non-empty for the leak test to mean anything.
func TestBundleIsNotVacuouslyClean(t *testing.T) {
	dir := withDataDir(t)
	writeFile(t, filepath.Join(dir, "unarr.log"), "2025-01-01 10:00:00 daemon started\n")

	b := Collect(Inputs{
		Config:  config.Default(),
		Version: "9.9.9",
		Doctor:  func() (doctor.Report, error) { return leakyDoctorReport(), nil },
		Logs:    testLogPaths(dir),
	})

	for _, name := range []string{"version.txt", "doctor.json", "config.redacted.toml", "unarr.log", "system.txt", "network.txt"} {
		if body := b.Body(name); len(body) == 0 {
			t.Errorf("section %s is empty or absent — the leak test would be meaningless", name)
		}
	}
	if !strings.Contains(string(b.Body("unarr.log")), "daemon started") {
		t.Error("the daemon log section did not carry the log line")
	}
	// The scrubber runs over the redacted config too (defence in depth). It must
	// not rewrite the projection's own markers: "<withheld>" and "<unset>" are
	// different answers, and collapsing them loses "the key IS configured".
	if !strings.Contains(string(b.Body("config.redacted.toml")), `api_key = "<unset>"`) {
		t.Errorf("the scrubber rewrote the projection's markers:\n%s", b.Body("config.redacted.toml"))
	}
}

// TestAbsentSectionsAreRecordedNotFatal is the degradation rule: a box with no
// daemon, no logs and no benchmark still produces a bundle, and every gap is
// reported WITH its reason. That box is the one whose bundle matters most.
func TestAbsentSectionsAreRecordedNotFatal(t *testing.T) {
	dir := withDataDir(t)

	b := Collect(Inputs{Config: config.Default(), Version: "9.9.9", Logs: testLogPaths(dir)})

	absent := map[string]string{}
	for _, s := range b.Sections {
		if s.Absent != "" {
			absent[s.Name] = s.Absent
		}
	}
	for _, name := range []string{"unarr.log", "unarr.err.log", "daemon.state.json", "tasks.json", "bench-encode.json", "doctor.json"} {
		if absent[name] == "" {
			t.Errorf("section %s should be recorded as absent with a reason", name)
		}
	}
	if b.Body("version.txt") == nil {
		t.Error("version.txt must still be collected when everything else is missing")
	}
	if !strings.Contains(b.Listing(), "unarr.log") {
		t.Errorf("the listing must show absent sections too:\n%s", b.Listing())
	}
}

// TestSectionsAreCapped checks the byte bound. The line cap alone does not
// bound the size — one log line may reach a megabyte — and the tail is what is
// kept, because the most recent output is what explains the failure.
func TestSectionsAreCapped(t *testing.T) {
	dir := withDataDir(t)
	defer withSectionCap(t, 4096)()
	// Many ordinary lines rather than one enormous one: the reader caps a single
	// line at 1 MiB, so a giant line would exercise the reader, not the byte cap.
	line := strings.Repeat("x", 100) + "\n"
	writeFile(t, filepath.Join(dir, "unarr.log"), strings.Repeat(line, 200)+"TAIL-MARKER\n")

	b := Collect(Inputs{Config: config.Default(), Version: "9.9.9", LogLines: 10000, Logs: testLogPaths(dir)})

	body := b.Body("unarr.log")
	if len(body) > maxSectionBytes+256 {
		t.Errorf("section not capped: %d bytes", len(body))
	}
	if !strings.Contains(string(body), "TAIL-MARKER") {
		t.Error("the cap dropped the tail; it must drop the head")
	}
	for _, s := range b.Sections {
		if s.Name == "unarr.log" && s.Note == "" {
			t.Error("a truncated section must say so in the manifest")
		}
	}
}

// TestWriteTarGzIsPrivateAndComplete checks the artefact itself: 0600, one
// top-level directory, a manifest, and no entry for a section that was absent.
func TestWriteTarGzIsPrivateAndComplete(t *testing.T) {
	dir := withDataDir(t)
	writeFile(t, filepath.Join(dir, "unarr.log"), "line one\n")

	b := Collect(Inputs{Config: config.Default(), Version: "9.9.9", Logs: testLogPaths(dir)})
	out := filepath.Join(t.TempDir(), DefaultName(time.Now()))
	if err := b.WriteTarGz(out); err != nil {
		t.Fatalf("write: %v", err)
	}

	st, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != bundleFileMode {
		t.Errorf("bundle mode = %o, want %o", st.Mode().Perm(), bundleFileMode)
	}

	names := tarEntries(t, out)
	root := strings.TrimSuffix(filepath.Base(out), ".tar.gz")
	if len(names) == 0 || names[0] != root+"/manifest.json" {
		t.Errorf("manifest.json must be the first entry, got %v", names)
	}
	for _, n := range names {
		if !strings.HasPrefix(n, root+"/") {
			t.Errorf("entry %q escapes the bundle directory", n)
		}
	}
	if slicesContains(names, root+"/bench-encode.json") {
		t.Error("an absent section must not produce a file")
	}
}

// TestDefaultNameIsUniquePerSecond keeps a second bundle from overwriting the
// first — support threads routinely collect a "before" and an "after".
func TestDefaultNameIsUniquePerSecond(t *testing.T) {
	a := DefaultName(time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC))
	b := DefaultName(time.Date(2025, 3, 4, 5, 6, 8, 0, time.UTC))
	if a == b {
		t.Fatalf("names collide: %s", a)
	}
	if !strings.HasPrefix(a, "unarr-support-") || !strings.HasSuffix(a, ".tar.gz") {
		t.Fatalf("unexpected name %q", a)
	}
}

// TestJournalHostReadsTheJournal covers the systemd branch: there is no
// unarr.log to read there, and a stale one must not be preferred over the live
// journal. The file-only sections say why they are missing.
func TestJournalHostReadsTheJournal(t *testing.T) {
	dir := withDataDir(t)
	writeFile(t, filepath.Join(dir, "unarr.log"), "STALE FILE FROM AN OLD RUN\n")

	b := Collect(Inputs{
		Config:  config.Default(),
		Version: "9.9.9",
		Logs:    testLogPaths(dir),
		Journal: func(w io.Writer, n int) error {
			_, err := io.WriteString(w, "LIVE JOURNAL LINE\n")
			return err
		},
	})

	got := string(b.Body("unarr.log"))
	if !strings.Contains(got, "LIVE JOURNAL LINE") || strings.Contains(got, "STALE FILE") {
		t.Errorf("journal host read the wrong source:\n%s", got)
	}
	for _, s := range b.Sections {
		if s.Name == "unarr.err.log" && !strings.Contains(s.Absent, "journal") {
			t.Errorf("unarr.err.log absence should explain the systemd case, got %q", s.Absent)
		}
	}
}

// TestBrokenCollectorDoesNotSinkTheBundle: a doctor run that explodes costs the
// bundle one section, not the command.
func TestBrokenCollectorDoesNotSinkTheBundle(t *testing.T) {
	dir := withDataDir(t)
	b := Collect(Inputs{
		Config:  config.Default(),
		Version: "9.9.9",
		Logs:    testLogPaths(dir),
		Doctor:  func() (doctor.Report, error) { return doctor.Report{}, errors.New("boom") },
	})
	for _, s := range b.Sections {
		if s.Name == "doctor.json" && !strings.Contains(s.Absent, "boom") {
			t.Errorf("doctor failure should be recorded as the absence reason, got %q", s.Absent)
		}
	}
	if b.Body("version.txt") == nil {
		t.Error("the rest of the bundle must still be collected")
	}
}

// ── helpers ────────────────────────────────────────────────────────────────

// withDataDir redirects every platform's data-dir resolver into a temp
// directory, so a test never reads — or writes — the developer's own agent.
// Mirrors the helper of the same name in internal/cmd.
func withDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("UNARR_CONFIG_DIR", dir) // macOS: data dir == config dir
	t.Setenv("XDG_DATA_HOME", dir)    // linux
	t.Setenv("LOCALAPPDATA", dir)     // windows

	got := config.DataDir()
	if !strings.HasPrefix(got, dir) {
		t.Skipf("config.DataDir() = %s on %s, not redirected into %s", got, runtime.GOOS, dir)
	}
	if err := os.MkdirAll(got, 0o755); err != nil {
		t.Fatalf("create data dir: %v", err)
	}
	return got
}

// withSectionCap shrinks the per-section byte cap for one test and restores it.
func withSectionCap(t *testing.T, n int) func() {
	t.Helper()
	prev := maxSectionBytes
	maxSectionBytes = n
	return func() { maxSectionBytes = prev }
}

func testLogPaths(dir string) LogPaths {
	return LogPaths{
		Daemon:   filepath.Join(dir, "unarr.log"),
		Err:      filepath.Join(dir, "unarr.err.log"),
		Boot:     filepath.Join(dir, "unarr.boot.log"),
		MaxFiles: 3,
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// populatedConfig is a Config with every string field carrying a marker:
// sentinelSecret in the classified-Secret fields, sentinelPublic everywhere
// else. See the constants for why the two differ.
func populatedConfig() config.Config {
	cfg := config.Default()
	fillStrings(reflect.ValueOf(&cfg).Elem(), sentinelPublic)
	v := reflect.ValueOf(&cfg).Elem()
	for _, path := range secretPaths() {
		fv := valueAt(v, path)
		if fv.IsValid() && fv.CanSet() && fv.Kind() == reflect.String {
			fv.SetString(sentinelSecret)
		}
	}
	// agent.id ships verbatim on purpose, so it gets its own marker. Leaving
	// sentinelPublic here would make the leak test fail for the one field that
	// is supposed to travel, and the obvious fix — exempting the marker — would
	// have quietly exempted every other field carrying it too.
	cfg.Agent.ID = sentinelAgentID
	return cfg
}

// leakyLogLines plants the marker in the shapes a credential actually takes in
// a daemon log.
func leakyLogLines() string {
	return strings.Join([]string{
		"2025/01/02 03:04:05 [api] GET /v1/agents?api_key=" + sentinelSecret,
		"2025/01/02 03:04:06 [stream] http://127.0.0.1:11818/hls/x.m3u8?t=" + sentinelSecret,
		"2025/01/02 03:04:07 [http] Authorization: Bearer " + sentinelSecret,
		"2025/01/02 03:04:08 [agent] registering with hash " + sentinelSecret,
		"2025/01/02 03:04:09 [vpn] key AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
	}, "\n") + "\n"
}

func leakyState() string {
	return `{"agentId":"` + sentinelSecret + `","status":"running","version":"1.0.0","pid":123,` +
		`"funnelUrl":"https://` + sentinelSecret + `.trycloudflare.com",` +
		`"vpnServer":"` + sentinelSecret + `:51820","logFile":"/home/` + sentinelPublic + `/unarr.log"}`
}

func leakyTasks() string {
	return `[{"id":"t1","title":"` + sentinelPublic + `","directUrl":"https://cdn/x?token=` + sentinelSecret + `",` +
		`"nzbPassword":"` + sentinelSecret + `","filePath":"/media/` + sentinelPublic + `.mkv","preferredMethod":"debrid"}]`
}

func leakyDoctorReport() doctor.Report {
	return doctor.Report{
		Status: doctor.StatusWarn,
		Checks: []doctor.Check{
			{Group: "Config", Name: "API key configured", Status: doctor.StatusPass, Message: sentinelSecret[:8] + "..."},
			{Group: "Config", Name: "Config file", Status: doctor.StatusPass, Message: "/home/tester/.config/unarr/config.toml"},
			{Group: "Downloads", Name: "Download dir", Status: doctor.StatusWarn, Message: "api_key=" + sentinelSecret},
		},
		Passed: 2, Warned: 1,
	}
}

func tarEntries(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	var names []string
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		names = append(names, h.Name)
	}
	return names
}

func slicesContains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

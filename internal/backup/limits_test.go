package backup

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// queueOf is an active-tasks.json of n debrid tasks, written the way the
// daemon's store writes it (real agent.Task values, MarshalIndent).
func queueOf(t *testing.T, n int) []byte {
	t.Helper()
	tasks := make([]agent.Task, n)
	for i := range tasks {
		id := fmt.Sprintf("task-%06d", i)
		tasks[i] = agent.Task{
			ID: id, InfoHash: fmt.Sprintf("%040x", i), Title: "Some.Show.S01E01.1080p.WEB-DL.DDP5.1.H.264-GROUP",
			PreferredMethod: "debrid", Mode: "download",
			DirectURL:      "https://debrid.example/d/" + id + "/0123456789abcdef0123456789abcdef",
			DirectFileName: "Some.Show.S01E01.1080p.WEB-DL.mkv", DirectFileSize: 3 << 30,
			ContentType: "show", ContentTitle: "Some Show",
			SourceSet: &agent.SourceSet{Version: 1, Sources: []agent.Source{
				{ID: "s-" + id, Transport: "debrid", Provider: "realdebrid", DirectURL: "https://debrid.example/s/" + id},
			}},
			QueuedAt: time.Unix(1_700_000_000+int64(i), 0).UTC(),
		}
	}
	data, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A queue of 40,000 tasks (~30 MB, well over the 16 MiB one member used to be
// allowed) must come back from create → restore: an archive create writes and
// restore refuses is the worst failure a backup can have.
func TestQueueOf40000TasksRoundTrips(t *testing.T) {
	queue := queueOf(t, 40_000)
	if len(queue) <= 16<<20 {
		t.Fatalf("queue is %d bytes: too small to cover the old 16 MiB member bound", len(queue))
	}
	for name, pass := range map[string]string{"encrypted": "pw", "secret-free": ""} {
		t.Run(name, func(t *testing.T) {
			t.Parallel() // slow under -race; neither touches maxContentSize
			l := newLayout(t)
			write(t, filepath.Join(l.DataDir, nameTasks), string(queue))
			res := mustCreate(t, l, CreateOptions{Passphrase: pass})
			arc, err := Open(res.Path, pass)
			if err != nil {
				t.Fatalf("restore refuses the archive create wrote: %v", err)
			}
			fresh := Layout{ConfigPath: filepath.Join(t.TempDir(), "config.toml"), DataDir: t.TempDir()}
			mustApply(t, arc, fresh, t.TempDir())
			got := []byte(read(t, filepath.Join(fresh.DataDir, nameTasks)))
			if pass != "" && !bytes.Equal(got, queue) {
				t.Fatal("encrypted round trip changed the queue")
			}
			var back []agent.Task
			if err := json.Unmarshal(got, &back); err != nil || len(back) != 40_000 || back[39_999].ID != "task-039999" {
				t.Fatalf("restored queue: %v, %d tasks", err, len(back))
			}
		})
	}
}

// withContentLimit shrinks the shared bound for one test, so the edge can be
// probed without writing 128 MiB.
func withContentLimit(t *testing.T, n int64) {
	t.Helper()
	old := maxContentSize
	maxContentSize = n
	t.Cleanup(func() { maxContentSize = old })
}

// jsonNoise is a queue of size bytes that passes checkTasks yet barely
// compresses: one task whose only field is random bytes, bar the few a JSON
// string cannot hold raw.
func jsonNoise(t *testing.T, size int) []byte {
	t.Helper()
	const head, tail = `[{"n":"`, `"}]`
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	for i, c := range b {
		if c < 0x20 || c == '"' || c == '\\' {
			b[i] = c | 0x80
		}
	}
	copy(b, head)
	copy(b[size-len(tail):], tail)
	return b
}

// Create refuses, before writing anything, the archive Open would refuse, and
// the file it does write opens — swept across the edge with all but
// incompressible content, where zip and encryption framing weigh the most.
func TestCreateWritesOnlyWhatOpenAccepts(t *testing.T) {
	const limit = 64 << 10
	withContentLimit(t, limit)
	l := newLayout(t)
	tasks := filepath.Join(l.DataDir, nameTasks)
	cfgSize := int64(len(testConfig))
	refused, written := 0, 0
	for size := limit - cfgSize - 1024; size <= limit-cfgSize; size += 256 {
		write(t, tasks, string(jsonNoise(t, int(size)))) // verbatim: an encrypted archive is not redacted
		dir := t.TempDir()
		res, err := Create(CreateOptions{Layout: l, Dir: dir, Passphrase: "pw"})
		if err != nil {
			if !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), nameTasks) {
				t.Fatalf("queue of %d bytes: err = %v, want ErrTooLarge naming %s", size, err, nameTasks)
			}
			if es, _ := os.ReadDir(dir); len(es) != 0 {
				t.Fatalf("queue of %d bytes: refused, yet %d file(s) written", size, len(es))
			}
			refused++
			continue
		}
		if _, err := Open(res.Path, "pw"); err != nil {
			t.Fatalf("queue of %d bytes: create wrote an archive Open refuses: %v", size, err)
		}
		written++
	}
	if refused == 0 || written == 0 {
		t.Fatalf("the sweep missed the edge: %d written, %d refused", written, refused)
	}
}

// Create refuses an oversized file without reading it whole, and an --out file
// is not created either.
func TestCreateRefusesOversizedFileBeforeWriting(t *testing.T) {
	withContentLimit(t, 64<<10)
	l := newLayout(t)
	f, err := os.Create(filepath.Join(l.DataDir, nameTasks))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 30); err != nil { // sparse: 1 GiB that is never read
		t.Fatal(err)
	}
	f.Close()
	out := filepath.Join(t.TempDir(), "b.zip")
	if _, err := Create(CreateOptions{Layout: l, Out: out, Passphrase: "pw"}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("--out written despite the refusal: %v", err)
	}
}

// The bound covers the members TOGETHER: two that each fit but not both are
// refused, so a crafted archive cannot multiply what a restore holds in memory.
func TestOpenBoundsTheMembersTogether(t *testing.T) {
	withContentLimit(t, 64<<10)
	half := strings.Repeat("x", 40<<10)
	m := `{"schema":1,"unarrVersion":"1","os":"linux","arch":"amd64","createdAt":"2026-01-01T00:00:00Z","secrets":"included","files":[` +
		fmt.Sprintf(`{"name":%q,"size":%d,"sha256":%q},`, nameConfig, len(half), sumHex([]byte(half))) +
		fmt.Sprintf(`{"name":%q,"size":%d,"sha256":%q}]}`, nameTasks, len(half), sumHex([]byte(half)))
	p := craft(t, map[string]string{ManifestName: m, nameConfig: half, nameTasks: half})
	if _, err := Open(p, ""); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

// wantTooLarge fails unless err is ErrTooLarge naming member.
func wantTooLarge(t *testing.T, err error, member string) {
	t.Helper()
	if !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), member) {
		t.Fatalf("err = %v, want ErrTooLarge naming %s", err, member)
	}
}

// Case 15 of the hostile-archive sweep: config.toml is 40 MiB of NUL bytes —
// 40 KB deflated — with a manifest that matches it. It fits the shared bound,
// so only config.toml's own cap stops restore from writing it as the config.
func TestOpenRefusesZipBombConfig(t *testing.T) {
	bomb := strings.Repeat("\x00", 40<<20)
	p := craft(t, map[string]string{ManifestName: manifestFor(nameConfig, bomb), nameConfig: bomb})
	if st, _ := os.Stat(p); st.Size() > 1<<20 {
		t.Fatalf("bomb archive is %d bytes: it should deflate to a few KB", st.Size())
	}
	_, err := Open(p, "")
	wantTooLarge(t, err, nameConfig)
}

// paddedConfig is testConfig grown with comment lines to exactly size bytes:
// still valid TOML, so only its size is in question.
func paddedConfig(size int) string {
	line := "# " + strings.Repeat("p", 77) + "\n"
	pad := size - len(testConfig)
	return testConfig + strings.Repeat(line, pad/len(line)) + strings.Repeat("\n", pad%len(line))
}

// A config.toml right at its cap backs up and restores, encrypted or not; one
// byte over, create refuses it before writing anything.
func TestConfigCapEdge(t *testing.T) {
	l := newLayout(t)
	atCap := paddedConfig(maxConfigSize)
	write(t, l.ConfigPath, atCap)
	for name, pass := range map[string]string{"encrypted": "pw", "secret-free": ""} {
		res := mustCreate(t, l, CreateOptions{Passphrase: pass})
		arc, err := Open(res.Path, pass)
		if err != nil {
			t.Fatalf("%s: a config at the cap does not open: %v", name, err)
		}
		fresh := Layout{ConfigPath: filepath.Join(t.TempDir(), "config.toml"), DataDir: t.TempDir()}
		mustApply(t, arc, fresh, t.TempDir())
		if got := read(t, fresh.ConfigPath); len(got) > maxConfigSize || !strings.HasPrefix(got, "# my comment\n[auth]") {
			t.Fatalf("%s: restored config is %d bytes, not the one backed up", name, len(got))
		}
	}

	write(t, l.ConfigPath, atCap+"\n")
	for name, pass := range map[string]string{"encrypted": "pw", "secret-free": ""} {
		dir, out := t.TempDir(), filepath.Join(t.TempDir(), "b.zip")
		_, err := Create(CreateOptions{Layout: l, Dir: dir, Passphrase: pass})
		wantTooLarge(t, err, nameConfig)
		_, err = Create(CreateOptions{Layout: l, Out: out, Passphrase: pass})
		wantTooLarge(t, err, nameConfig)
		if es, _ := os.ReadDir(dir); len(es) != 0 {
			t.Fatalf("%s: refused, yet %d file(s) written", name, len(es))
		}
		if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s: --out written despite the refusal: %v", name, err)
		}
	}
}

// Open refuses a member by the size its header declares, before inflating it:
// config.toml declares 5 MiB, over its own 4 MiB cap yet well inside what is
// left of the shared bound, and its deflate stream is corrupt. Checked first,
// that is ErrTooLarge; inflated first, it would be ErrChecksum.
func TestOpenRefusesDeclaredSizeBeforeInflating(t *testing.T) {
	garbage := bytes.Repeat([]byte{0xff}, 64) // BTYPE=11: corrupt from its first block
	p := filepath.Join(t.TempDir(), "declared.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.CreateRaw(&zip.FileHeader{Name: nameConfig, Method: zip.Deflate,
		UncompressedSize64: 5 << 20, CompressedSize64: uint64(len(garbage))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(garbage); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	_, err = Open(p, "")
	wantTooLarge(t, err, nameConfig)
	if errors.Is(err, ErrChecksum) {
		t.Fatalf("err = %v: the member was inflated before its declared size was checked", err)
	}
}

// Open holds config.toml and the manifest to their own caps too, whatever
// create would have done: one byte over is refused, valid content or not.
func TestOpenRefusesMembersOverTheirOwnCap(t *testing.T) {
	cfg := paddedConfig(maxConfigSize + 1)
	_, err := Open(craft(t, map[string]string{ManifestName: manifestFor(nameConfig, cfg), nameConfig: cfg}), "")
	wantTooLarge(t, err, nameConfig)

	// Whitespace keeps the manifest valid JSON while it grows past its cap.
	m := manifestFor(nameConfig, testConfig)
	m = m[:len(m)-1] + strings.Repeat(" ", maxManifestSize-len(m)+1) + "}"
	_, err = Open(craft(t, map[string]string{ManifestName: m, nameConfig: testConfig}), "")
	wantTooLarge(t, err, ManifestName)
}

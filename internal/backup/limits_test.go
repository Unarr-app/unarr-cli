package backup

import (
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

// Create refuses, before writing anything, the archive Open would refuse, and
// the file it does write opens — swept across the edge with incompressible
// content, where zip and encryption framing weigh the most.
func TestCreateWritesOnlyWhatOpenAccepts(t *testing.T) {
	const limit = 64 << 10
	withContentLimit(t, limit)
	l := newLayout(t)
	tasks := filepath.Join(l.DataDir, nameTasks)
	cfgSize := int64(len(testConfig))
	refused, written := 0, 0
	for size := limit - cfgSize - 1024; size <= limit-cfgSize; size += 256 {
		noise := make([]byte, size)
		if _, err := rand.Read(noise); err != nil {
			t.Fatal(err)
		}
		write(t, tasks, string(noise)) // verbatim: an encrypted archive does not parse the queue
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

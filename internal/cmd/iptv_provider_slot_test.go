package cmd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/engine"
)

// connCounter is a fake IPTV provider: it serves one file with HTTP ranges,
// throttled so a reader stays connected for seconds, and records the highest
// number of requests it ever served at once.
type connCounter struct {
	path    string
	active  atomic.Int32
	maxSeen atomic.Int32
}

func (c *connCounter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := c.active.Add(1)
	defer c.active.Add(-1)
	for {
		m := c.maxSeen.Load()
		if n <= m || c.maxSeen.CompareAndSwap(m, n) {
			break
		}
	}
	data, err := os.ReadFile(c.path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	start := 0
	if rg := r.Header.Get("Range"); strings.HasPrefix(rg, "bytes=") {
		from, _, _ := strings.Cut(strings.TrimPrefix(rg, "bytes="), "-")
		if v, err := strconv.Atoi(from); err == nil && v < len(data) {
			start = v
		}
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", "video/x-matroska")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)-start))
	if start > 0 || r.Header.Get("Range") != "" {
		w.Header().Set("Content-Range", "bytes "+strconv.Itoa(start)+"-"+strconv.Itoa(len(data)-1)+"/"+strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusPartialContent)
	}
	if r.Method == http.MethodHead {
		return
	}
	const chunk = 8 << 10
	for off := start; off < len(data); off += chunk {
		end := min(off+chunk, len(data))
		if _, err := w.Write(data[off:end]); err != nil {
			return
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func testFFmpeg(t *testing.T) (ffmpeg, ffprobe string) {
	t.Helper()
	var err error
	if ffmpeg, err = exec.LookPath("ffmpeg"); err != nil {
		t.Skipf("ffmpeg not on PATH: %v", err)
	}
	if ffprobe, err = exec.LookPath("ffprobe"); err != nil {
		t.Skipf("ffprobe not on PATH: %v", err)
	}
	return ffmpeg, ffprobe
}

// B1: every session transition (Retry, next episode, quality switch, takeover)
// closes the old IPTV session and starts the new one back to back, and the web
// close tears the old one down OFF the sync loop. The provider must still never
// see the two at once: the new session waits for the old ffmpeg to be reaped.
func TestIptvSessionTransitionKeepsOneProviderConnection(t *testing.T) {
	ffmpeg, ffprobe := testFFmpeg(t)
	tmp := t.TempDir()
	src := filepath.Join(tmp, "ep.mkv")
	if out, err := exec.Command(ffmpeg, "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=60:size=640x360:rate=25",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=60",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "50", "-pix_fmt", "yuv420p",
		"-c:a", "aac", src).CombinedOutput(); err != nil {
		t.Fatalf("generate: %v\n%s", err, out)
	}

	for _, tc := range []struct {
		name     string
		withSlot bool
		want     int32
	}{
		// Without the slot the measurement reproduces the review: 2 at once.
		{"without the provider slot", false, 2},
		{"with the provider slot", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &connCounter{path: src}
			ts := httptest.NewServer(provider)
			defer ts.Close()
			reg := engine.NewHLSSessionRegistry(1)
			defer reg.CloseWhere(func(*engine.HLSSession) bool { return true })
			var slot *providerStreamSlot
			if tc.withSlot {
				slot = newProviderStreamSlots(func(id string) {
					closePlayerSessionsByWeb([]string{id}, reg)
				}).forURL(ts.URL + "/movie/u/p/1.mkv")
			}
			dl := engine.NewIptvDownloader(engine.NewPlaybackHold(time.Minute))
			prefix := "b1" + strconv.FormatBool(tc.withSlot)

			start := func(id string) *engine.HLSSession {
				ctx, cancel := context.WithCancel(context.Background())
				playerSessionRegistry.add(id, cancel)
				s, err := engine.StartHLSSession(ctx, engine.HLSSessionConfig{
					SessionID:        id,
					SourceURL:        ts.URL + "/movie/u/p/1.mkv",
					SingleConnection: true,
					Quality:          "360p",
					AcquireSource: func(actx context.Context) (func(), error) {
						return holdIptvForStream(actx, dl, slot, id)
					},
					Transcode: engine.TranscodeRuntime{FFmpegPath: ffmpeg, FFprobePath: ffprobe, Preset: "ultrafast"},
				})
				if err != nil {
					t.Fatalf("start %s: %v", id, err)
				}
				reg.Register(s)
				return s
			}

			start(prefix + "-a")
			// Let A's ffmpeg settle into its (throttled) read.
			deadline := time.Now().Add(5 * time.Second)
			for provider.active.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			time.Sleep(300 * time.Millisecond)
			provider.maxSeen.Store(provider.active.Load())

			// The daemon's exact order: the sync's closed ids, then the new session.
			// The teardown runs off the sync loop; here it is also SLOW to get going
			// (a busy host, a NAS tmpdir), which is what the new session must not
			// depend on.
			webSessionCloser{
				registry: playerSessionRegistry,
				closed:   closedPlayerSessions,
				hls:      hlsRegistryCloser{reg: reg},
				run: func(f func()) {
					go func() {
						time.Sleep(300 * time.Millisecond)
						f()
					}()
				},
			}.closeByWeb([]string{prefix + "-a"})
			start(prefix + "-b")
			time.Sleep(500 * time.Millisecond)

			if got := provider.maxSeen.Load(); got != tc.want {
				t.Fatalf("provider saw %d concurrent connections across the transition, want %d", got, tc.want)
			}
		})
	}
}

// A session that is still STARTING (probing) when a newer one arrives is
// preempted too: its start is cancelled and the slot freed without waiting for
// the full bound.
func TestProviderSlotPreemptsTheHolder(t *testing.T) {
	var preempted atomic.Value
	var slot *providerStreamSlot
	var releaseA func()
	slot = newProviderStreamSlot(func(id string) {
		preempted.Store(id)
		releaseA() // the holder's teardown gives the slot back
	})
	releaseA = mustAcquire(t, slot, "a", time.Second)

	started := time.Now()
	releaseB := mustAcquire(t, slot, "b", 5*time.Second)
	defer releaseB()
	if got, _ := preempted.Load().(string); got != "a" {
		t.Fatalf("preempted %q, want the holder a", got)
	}
	if waited := time.Since(started); waited > time.Second {
		t.Fatalf("b waited %s for a preempted holder", waited)
	}
}

// The same session id never preempts itself, and a holder that never lets go
// only delays the newcomer by the bound — it plays rather than hanging.
func TestProviderSlotBoundedWait(t *testing.T) {
	slot := newProviderStreamSlot(func(string) {})
	releaseA := mustAcquire(t, slot, "a", time.Second)
	defer releaseA()
	started := time.Now()
	release := mustAcquire(t, slot, "b", 100*time.Millisecond)
	release()
	if waited := time.Since(started); waited < 100*time.Millisecond || waited > 2*time.Second {
		t.Fatalf("bounded wait took %s", waited)
	}
	// The no-op release must not have freed a's slot.
	select {
	case slot.sem <- struct{}{}:
		t.Fatal("a no-op release freed the holder's slot")
	default:
	}
}

func mustAcquire(t *testing.T, slot *providerStreamSlot, id string, wait time.Duration) func() {
	t.Helper()
	release, err := slot.acquire(context.Background(), id, wait)
	if err != nil {
		t.Fatalf("acquire %s: %v", id, err)
	}
	return release
}

// Three sessions in one teardown window (double Retry, next episode twice): A
// holds, B arrives and preempts A, C arrives before A has let go. Exactly one
// newcomer — the newest, C — gets the provider; B gives up without reading it,
// and nobody sits out the full bound. Before, B and C both waited, one won and
// the other "opened anyway" after 10 s: two connections.
func TestProviderSlotNewestOfThreeWins(t *testing.T) {
	var (
		holding  atomic.Int32 // sessions currently past acquire (reading the provider)
		peak     atomic.Int32
		preempts atomic.Int32
	)
	enter := func() {
		n := holding.Add(1)
		for {
			m := peak.Load()
			if n <= m || peak.CompareAndSwap(m, n) {
				return
			}
		}
	}
	releases := map[string]func(){}
	var relMu sync.Mutex
	var slot *providerStreamSlot
	slot = newProviderStreamSlot(func(id string) {
		preempts.Add(1)
		// A slow teardown: the holder lets go only after both newcomers queued.
		time.Sleep(150 * time.Millisecond)
		relMu.Lock()
		rel := releases[id]
		delete(releases, id) // a second preempt of the same holder is a no-op
		relMu.Unlock()
		if rel != nil {
			holding.Add(-1)
			rel()
		}
	})
	relMu.Lock()
	releases["a"] = mustAcquire(t, slot, "a", time.Second)
	relMu.Unlock()
	enter()

	type result struct {
		id      string
		err     error
		elapsed time.Duration
	}
	results := make(chan result, 2)
	acquire := func(id string) {
		start := time.Now()
		rel, err := slot.acquire(context.Background(), id, 10*time.Second)
		if err == nil {
			enter()
			relMu.Lock()
			releases[id] = rel
			relMu.Unlock()
		}
		results <- result{id, err, time.Since(start)}
	}
	go acquire("b")
	time.Sleep(40 * time.Millisecond)
	go acquire("c")

	got := map[string]result{}
	for i := 0; i < 2; i++ {
		r := <-results
		got[r.id] = r
	}
	if got["c"].err != nil {
		t.Fatalf("newest session c did not get the provider: %v", got["c"].err)
	}
	if !errors.Is(got["b"].err, errProviderSuperseded) {
		t.Fatalf("overtaken session b: err=%v, want errProviderSuperseded", got["b"].err)
	}
	if !errors.Is(got["b"].err, engine.ErrSourceUnreachable) {
		t.Fatal("superseded must report as source_unreachable")
	}
	if p := peak.Load(); p != 1 {
		t.Fatalf("%d sessions read the provider at once, want 1", p)
	}
	for id, r := range got {
		if r.elapsed > 2*time.Second {
			t.Fatalf("%s waited %s: somebody sat out the bound", id, r.elapsed)
		}
	}
	slot.mu.Lock()
	holder := slot.holder
	slot.mu.Unlock()
	if holder != "c" {
		t.Fatalf("slot holder %q, want c", holder)
	}
}

// A session cancelled while it waits (the web closed it) never reads the
// provider: it returns ctx's error instead of "opening anyway".
func TestProviderSlotCancelledWaiterDoesNotOpen(t *testing.T) {
	slot := newProviderStreamSlot(func(string) {})
	releaseA := mustAcquire(t, slot, "a", time.Second)
	defer releaseA()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	release, err := slot.acquire(ctx, "b", 5*time.Second)
	if err == nil {
		release()
		t.Fatal("a cancelled waiter got a (no-op) release and would read the provider")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want the ctx error", err)
	}
}

func TestProviderAccountKey(t *testing.T) {
	a := providerAccountKey("http://panel.example:8080/movie/alice/secret/1.mkv")
	b := providerAccountKey("http://panel.example:8080/series/alice/secret/9.mp4")
	c := providerAccountKey("http://panel.example:8080/movie/bob/other/1.mkv")
	d := providerAccountKey("http://panel.example:8080/get.php?username=alice&password=x")
	if a != b {
		t.Fatalf("same account, different keys: %q vs %q", a, b)
	}
	if a == c {
		t.Fatal("two accounts on one panel share a slot")
	}
	if d != a {
		t.Fatalf("query-form account key %q, want %q", d, a)
	}
	if strings.Contains(a, "secret") {
		t.Fatal("account key carries the password")
	}
}

package cmd

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/engine"
)

// fakeHLSCloser records closeSession calls; ids in live are "registered".
type fakeHLSCloser struct {
	mu     sync.Mutex
	live   map[string]bool
	closed []string
}

func (f *fakeHLSCloser) closeSession(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.live[id] {
		return false
	}
	delete(f.live, id)
	f.closed = append(f.closed, id)
	return true
}

func TestWebSessionCloser_CloseByWeb(t *testing.T) {
	tests := []struct {
		name        string
		registered  []string
		hlsLive     []string
		calls       [][]string // successive syncs
		wantTorn    []int      // closeByWeb result per call
		wantCancels map[string]int
		wantHLS     []string
		wantLeft    []string // ids still in the player registry
		wantClosed  []string // ids that must be in the recently-closed set
		notClosed   []string
	}{
		{
			name:       "unknown id is a no-op but is remembered as closed",
			calls:      [][]string{{"ghost"}},
			wantTorn:   []int{0},
			wantClosed: []string{"ghost"},
		},
		{
			name:        "running HLS session is cancelled, closed and removed",
			registered:  []string{"a", "b"},
			hlsLive:     []string{"a", "b"},
			calls:       [][]string{{"a"}},
			wantTorn:    []int{1},
			wantCancels: map[string]int{"a": 1},
			wantHLS:     []string{"a"},
			wantLeft:    []string{"b"},
			wantClosed:  []string{"a"},
			notClosed:   []string{"b"},
		},
		{
			name:        "idempotent across repeated syncs",
			registered:  []string{"a"},
			hlsLive:     []string{"a"},
			calls:       [][]string{{"a"}, {"a"}, {"a", "a"}},
			wantTorn:    []int{1, 0, 0},
			wantCancels: map[string]int{"a": 1},
			wantHLS:     []string{"a"},
			wantClosed:  []string{"a"},
		},
		{
			name:        "HLS still starting (no engine session yet) only cancels",
			registered:  []string{"a"},
			calls:       [][]string{{"a"}},
			wantTorn:    []int{1},
			wantCancels: map[string]int{"a": 1},
			wantClosed:  []string{"a"},
		},
		{
			name:     "empty id is ignored",
			calls:    [][]string{{""}},
			wantTorn: []int{0},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reg := &playerSessionRegistryT{cancels: make(map[string]context.CancelFunc)}
			cancels := map[string]int{}
			for _, id := range tc.registered {
				reg.add(id, func() { cancels[id]++ })
			}
			hls := &fakeHLSCloser{live: map[string]bool{}}
			for _, id := range tc.hlsLive {
				hls.live[id] = true
			}
			c := webSessionCloser{
				registry: reg,
				closed:   newRecentlyClosedSessions(16),
				hls:      hls,
				run:      func(f func()) { f() },
			}
			for i, ids := range tc.calls {
				if got := c.closeByWeb(ids); got != tc.wantTorn[i] {
					t.Errorf("call %d: torn down = %d, want %d", i, got, tc.wantTorn[i])
				}
			}
			for id, want := range tc.wantCancels {
				if cancels[id] != want {
					t.Errorf("cancel(%s) ran %d times, want %d", id, cancels[id], want)
				}
			}
			for id, got := range cancels {
				if _, ok := tc.wantCancels[id]; !ok && got > 0 {
					t.Errorf("cancel(%s) ran %d times, want 0", id, got)
				}
			}
			if len(hls.closed) != len(tc.wantHLS) {
				t.Errorf("HLS closed = %v, want %v", hls.closed, tc.wantHLS)
			}
			if reg.count() != len(tc.wantLeft) {
				t.Errorf("registry count = %d, want %d", reg.count(), len(tc.wantLeft))
			}
			for _, id := range tc.wantLeft {
				if !reg.has(id) {
					t.Errorf("registry lost %s", id)
				}
			}
			for _, id := range tc.wantClosed {
				if !c.closed.contains(id) {
					t.Errorf("%s not remembered as closed", id)
				}
			}
			for _, id := range tc.notClosed {
				if c.closed.contains(id) {
					t.Errorf("%s wrongly marked closed", id)
				}
			}
		})
	}
}

func TestRecentlyClosedSessions_Bounded(t *testing.T) {
	r := newRecentlyClosedSessions(3)
	for _, id := range []string{"a", "b", "c"} {
		if !r.mark(id) {
			t.Fatalf("mark(%s) should be new", id)
		}
	}
	if r.mark("a") {
		t.Fatal("re-marking a known id must report not-new")
	}
	r.mark("d") // evicts the oldest ("a")
	tests := []struct {
		id   string
		want bool
	}{{"a", false}, {"b", true}, {"c", true}, {"d", true}}
	for _, tc := range tests {
		if got := r.contains(tc.id); got != tc.want {
			t.Errorf("contains(%s) = %v, want %v", tc.id, got, tc.want)
		}
	}
	if len(r.order) != 3 || len(r.set) != 3 {
		t.Errorf("size = order %d / set %d, want 3", len(r.order), len(r.set))
	}
}

// asyncCloser is a webSessionCloser over a REAL registry that runs teardowns in
// goroutines, as production does; wait() joins them.
type asyncCloser struct {
	webSessionCloser
	wg *sync.WaitGroup
}

func newAsyncCloser() asyncCloser {
	wg := &sync.WaitGroup{}
	return asyncCloser{
		webSessionCloser: webSessionCloser{
			registry: &playerSessionRegistryT{cancels: make(map[string]context.CancelFunc)},
			closed:   newRecentlyClosedSessions(maxRecentlyClosedSessions),
			hls:      &fakeHLSCloser{live: map[string]bool{}},
			run:      func(f func()) { wg.Go(f) },
		},
		wg: wg,
	}
}

func (a asyncCloser) serve(srv *engine.StreamServer, id, taskID string, release func()) bool {
	gen := srv.SetFile(&fakeStreamProvider{size: 1024}, taskID)
	return a.registry.registerServed(id, slotSessionCancel(srv, gen, release), a.closed)
}

// Retry on a direct-play session: the web closes O and creates N in the same
// sync. O's teardown runs in a goroutine AFTER N's synchronous SetFile landed —
// it must never clear N's file.
func TestCloseByWeb_OldSlotTeardownNeverClearsNewerFile(t *testing.T) {
	srv := engine.NewStreamServer(0, 1)
	a := newAsyncCloser()
	for i := range 300 {
		oldID, newID := fmt.Sprintf("old-%d", i), fmt.Sprintf("new-%d", i)
		if !a.serve(srv, oldID, "task", nil) {
			t.Fatalf("iter %d: old session reported closed before any close", i)
		}
		a.closeByWeb([]string{oldID})
		if !a.serve(srv, newID, "task-new", nil) {
			t.Fatalf("iter %d: new session reported closed", i)
		}
		a.wg.Wait()
		if !srv.HasFile() || srv.CurrentTaskID() != "task-new" {
			t.Fatalf("iter %d: the old session's teardown cleared the new one (has=%v task=%q)",
				i, srv.HasFile(), srv.CurrentTaskID())
		}
		// Closing the session that still owns the slot does clear it.
		a.closeByWeb([]string{newID})
		a.wg.Wait()
		if srv.HasFile() {
			t.Fatalf("iter %d: closing the current owner left /stream serving", i)
		}
	}
}

// A VLC / task stream that took /stream outside the player registry survives the
// close of the player session it displaced; the task's own stop still clears it.
func TestCloseByWeb_PlayerCloseKeepsTaskStream(t *testing.T) {
	srv := engine.NewStreamServer(0, 1)
	a := newAsyncCloser()
	a.serve(srv, "player-d", "task-x", nil)
	setTaskStreamGen("vlc-task", srv.SetFile(&fakeStreamProvider{size: 2048}, "vlc-task"))

	a.closeByWeb([]string{"player-d"})
	a.wg.Wait()
	if !srv.HasFile() || srv.CurrentTaskID() != "vlc-task" {
		t.Fatalf("closing the player session cut the VLC stream (has=%v task=%q)", srv.HasFile(), srv.CurrentTaskID())
	}

	clearTaskStream(srv, "vlc-task")
	if srv.HasFile() {
		t.Fatal("stopping the task that owns /stream must clear it")
	}
}

// A task stop never clears a browser player session that serves the same task id
// since (the old CurrentTaskID()==taskID match did).
func TestClearTaskStream_KeepsPlayerSessionOfSameTask(t *testing.T) {
	srv := engine.NewStreamServer(0, 1)
	a := newAsyncCloser()
	setTaskStreamGen("shared-task", srv.SetFile(&fakeStreamProvider{size: 10}, "shared-task"))
	a.serve(srv, "player-p", "shared-task", nil)

	clearTaskStream(srv, "shared-task")
	if !srv.HasFile() {
		t.Fatal("a task stop cleared the player session that superseded it")
	}
}

// A superseded slot session (usenet handle / remux ffmpeg) still releases its
// OWN resources when the web closes it — only the slot clear is skipped.
func TestCloseByWeb_SupersededSlotStillReleases(t *testing.T) {
	srv := engine.NewStreamServer(0, 1)
	a := newAsyncCloser()
	var released atomic.Int32
	a.serve(srv, "usenet-old", "t-old", func() { released.Add(1) })
	a.serve(srv, "usenet-new", "t-new", nil)

	a.closeByWeb([]string{"usenet-old", "usenet-old"})
	a.wg.Wait()
	if got := released.Load(); got != 1 {
		t.Fatalf("superseded session released %d times, want 1 (usenet handle leaked)", got)
	}
	if srv.CurrentTaskID() != "t-new" {
		t.Fatalf("superseded close cleared the newer session (task=%q)", srv.CurrentTaskID())
	}
}

// A close landing after the start path's last check but before its registration
// found only the placeholder: registerServed must tear the session down itself.
func TestRegisterServed_CloseInTheWindowTearsDown(t *testing.T) {
	srv := engine.NewStreamServer(0, 1)
	a := newAsyncCloser()
	var probeAborted, released atomic.Int32
	a.registry.add("s", func() { probeAborted.Add(1) }) // placeholder (probe cancel)
	a.closeByWeb([]string{"s"})
	a.wg.Wait()

	if a.serve(srv, "s", "t", func() { released.Add(1) }) {
		t.Fatal("registerServed reported a web-closed session as live")
	}
	if srv.HasFile() || a.registry.has("s") {
		t.Fatalf("closed session still served/registered (has=%v reg=%v)", srv.HasFile(), a.registry.has("s"))
	}
	if probeAborted.Load() != 1 || released.Load() != 1 {
		t.Fatalf("probe aborted %d / released %d, want 1/1", probeAborted.Load(), released.Load())
	}
}

// Usenet direct: a session the web closed during NNTP setup is never served and
// does not re-register.
func TestServeUsenetDirect_ClosedDuringSetupIsNotServed(t *testing.T) {
	srv := engine.NewStreamServer(0, 1)
	id := "usenet-closed-during-setup"
	playerSessionRegistry.add(id, func() {}) // setup placeholder
	closePlayerSessionsByWeb([]string{id}, srv.HLS())

	ready := false
	d := usenetStreamDeps{streamSrv: srv, markReady: func(string) { ready = true }}
	d.serveUsenetDirect(agent.StreamSession{SessionID: id, TaskID: "t"},
		&engine.UsenetStreamHandle{Provider: &fakeStreamProvider{size: 10}, VideoName: "v.mkv"})

	if srv.HasFile() || ready || playerSessionRegistry.has(id) {
		t.Fatalf("closed usenet session served=%v ready=%v registered=%v", srv.HasFile(), ready, playerSessionRegistry.has(id))
	}
}

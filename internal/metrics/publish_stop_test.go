package metrics

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// waitPosts waits until poster has been handed n snapshots.
func waitPosts(t *testing.T, poster *fakePoster, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for poster.count() < n && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if poster.count() < n {
		t.Fatalf("%d posts, want %d", poster.count(), n)
	}
}

// stopWithin runs stop and fails if it holds the caller longer than limit.
func stopWithin(t *testing.T, stop func(), limit time.Duration) {
	t.Helper()
	done := make(chan struct{})
	start := time.Now()
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("stop held the caller for over %s", limit)
	}
	if took := time.Since(start); took > limit {
		t.Fatalf("stop took %s, want under %s", took, limit)
	}
}

// A clean stop clears the web's view at once: ONE empty snapshot goes out, so
// a stopped daemon's sessions do not linger until they go stale. Stopping again
// sends nothing more.
func TestPublisherStopSendsFinalEmptySnapshot(t *testing.T) {
	sessions := []Session{session("a")}
	poster := &fakePoster{}
	p, _ := pubWith(&sessions, poster)

	stop := p.Start(context.Background())
	waitPosts(t, poster, 1)
	stop()
	if poster.count() != 2 {
		t.Fatalf("%d posts, want the snapshot and one final empty one", poster.count())
	}
	if last := poster.last(t); last.Sessions == nil || len(last.Sessions) != 0 {
		t.Fatalf("final snapshot carries %v, want sessions: []", last.Sessions)
	}
	stop()
	if poster.count() != 2 {
		t.Fatalf("a second stop posted again: %d posts", poster.count())
	}
}

// The daemon cancels its context before the deferred stop runs: the final
// snapshot must still go out, on a context of its own.
func TestPublisherStopAfterContextEndsStillClears(t *testing.T) {
	sessions := []Session{session("a")}
	poster := &fakePoster{}
	p, _ := pubWith(&sessions, poster)

	ctx, cancel := context.WithCancel(context.Background())
	stop := p.Start(ctx)
	waitPosts(t, poster, 1)
	cancel()
	stop()
	if poster.count() != 2 || len(poster.last(t).Sessions) != 0 {
		t.Fatalf("%d posts, want the final empty snapshot after the context ended", poster.count())
	}
}

// The final snapshot is a real empty view: nothing active, no rate, but the
// monotonic totals and the agent kept, so the web need not special-case it.
func TestPublisherFinalSnapshotShape(t *testing.T) {
	poster := &fakePoster{}
	p := NewPublisher(func(now time.Time) Snapshot {
		return Snapshot{
			SchemaVersion: SchemaVersion, GeneratedAt: now, Agent: Agent{Version: "9.9.9"},
			Sessions: []Session{session("a")},
			Summary: Summary{
				Active: map[Kind]int{KindDownload: 1}, Stalled: 1,
				RateBps: map[Kind]int64{KindDownload: 500}, BytesTotal: map[Kind]int64{KindDownload: 7},
			},
		}
	}, poster)
	p.Logf = func(string, ...any) {}

	stop := p.Start(context.Background())
	waitPosts(t, poster, 1)
	stop()
	last := poster.last(t)
	if last.Agent.Version != "9.9.9" || last.SchemaVersion != SchemaVersion || last.Summary.BytesTotal[KindDownload] != 7 {
		t.Fatalf("final snapshot lost the agent or the totals: %+v", last)
	}
	if last.Summary.Stalled != 0 {
		t.Fatalf("final snapshot still counts %d stalled", last.Summary.Stalled)
	}
	for _, k := range Kinds {
		if last.Summary.Active[k] != 0 || last.Summary.RateBps[k] != 0 {
			t.Fatalf("final snapshot: %s active %d rate %d, want 0", k, last.Summary.Active[k], last.Summary.RateBps[k])
		}
	}
}

// Nothing to clear: the web already holds an empty view.
func TestPublisherStopSkipsWhenAlreadyEmpty(t *testing.T) {
	sessions := []Session{}
	poster := &fakePoster{}
	p, _ := pubWith(&sessions, poster)

	stop := p.Start(context.Background())
	waitPosts(t, poster, 1)
	stop()
	if poster.count() != 1 {
		t.Fatalf("%d posts, want no final snapshot after an empty one", poster.count())
	}
}

// A web without the endpoint is not asked again on the way out.
func TestPublisherStopSkipsUnsupportedWeb(t *testing.T) {
	sessions := []Session{session("a")}
	poster := &fakePoster{err: fmt.Errorf("%w (HTTP 404)", ErrPublishUnsupported)}
	p, _ := pubWith(&sessions, poster)

	stop := p.Start(context.Background())
	waitPosts(t, poster, 1)
	stop()
	if poster.count() != 1 {
		t.Fatalf("%d posts, want no final snapshot to a web that answered 404", poster.count())
	}
}

// hangingPoster accepts the first snapshot, then never answers — not even to
// its context, the worst a Poster can do.
type hangingPoster struct {
	mu      sync.Mutex
	calls   int
	release chan struct{}
}

func (h *hangingPoster) PostSessions(context.Context, []byte) error {
	h.mu.Lock()
	h.calls++
	first := h.calls == 1
	h.mu.Unlock()
	if !first {
		<-h.release
	}
	return nil
}

func (h *hangingPoster) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

// A slow or dead server costs the shutdown at most StopTimeout (2 s by
// default), whether or not the Poster honours its context.
func TestPublisherStopBoundedBySlowServer(t *testing.T) {
	if PublishStopTimeout > 2*time.Second {
		t.Fatalf("PublishStopTimeout = %s, want at most 2s", PublishStopTimeout)
	}
	sessions := []Session{session("a")}
	poster := &hangingPoster{release: make(chan struct{})}
	defer close(poster.release)
	p, _ := pubWith(&sessions, poster)
	p.StopTimeout = 50 * time.Millisecond

	stop := p.Start(context.Background())
	deadline := time.Now().Add(2 * time.Second)
	for poster.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	stopWithin(t, stop, time.Second)
	if poster.count() != 2 {
		t.Fatalf("%d posts, want the final snapshot attempted", poster.count())
	}
}

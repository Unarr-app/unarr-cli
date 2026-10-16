package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePoster records every snapshot it is handed and answers with err.
type fakePoster struct {
	mu     sync.Mutex
	bodies [][]byte
	err    error
	block  bool // wait for the request context instead of answering
}

func (f *fakePoster) PostSessions(ctx context.Context, snapshot []byte) error {
	f.mu.Lock()
	f.bodies = append(f.bodies, append([]byte(nil), snapshot...))
	block, err := f.block, f.err
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	return err
}

func (f *fakePoster) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakePoster) last(t *testing.T) Snapshot {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		t.Fatal("nothing was posted")
	}
	var s Snapshot
	if err := json.Unmarshal(f.bodies[len(f.bodies)-1], &s); err != nil {
		t.Fatalf("posted body is not a snapshot: %v", err)
	}
	return s
}

func (f *fakePoster) setErr(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

// logSink collects what the publisher logs.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *logSink) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.lines)
}

// pubWith returns a publisher whose source reports *sessions.
func pubWith(sessions *[]Session, poster Poster) (*Publisher, *logSink) {
	sink := &logSink{}
	p := NewPublisher(func(now time.Time) Snapshot {
		return Snapshot{SchemaVersion: SchemaVersion, GeneratedAt: now, Sessions: *sessions}
	}, poster)
	p.Logf = sink.logf
	return p, sink
}

func session(id string) Session {
	return Session{ID: id, Kind: KindDownload, Title: "Some.Show.S01E01", State: "downloading"}
}

// While anything is moving the snapshot goes out on every 5 s tick; once the
// view empties exactly ONE empty snapshot is sent, then one per 30 s heartbeat.
// The local poll keeps its 5 s tick in idle so a new session is not 30 s late.
func TestPublisherCadence(t *testing.T) {
	ctx := context.Background()
	sessions := []Session{session("a")}
	poster := &fakePoster{}
	p, _ := pubWith(&sessions, poster)

	for i := 0; i < 3; i++ {
		if wait := p.tick(ctx, at(i*5)); wait != PublishActiveInterval {
			t.Fatalf("active tick %d waits %s, want %s", i, wait, PublishActiveInterval)
		}
	}
	if poster.count() != 3 {
		t.Fatalf("active: %d posts in 3 ticks, want 3", poster.count())
	}

	sessions = nil
	p.tick(ctx, at(15)) // the view just emptied: one empty snapshot
	if poster.count() != 4 || len(poster.last(t).Sessions) != 0 {
		t.Fatalf("emptying: %d posts, want the 4th to be an empty snapshot", poster.count())
	}
	if poster.last(t).Sessions == nil {
		t.Fatal("empty snapshot must carry sessions: [], not null")
	}
	for s := 20; s < 45; s += 5 {
		if wait := p.tick(ctx, at(s)); wait != PublishActiveInterval {
			t.Fatalf("idle tick waits %s, want the %s local poll", wait, PublishActiveInterval)
		}
	}
	if poster.count() != 4 {
		t.Fatalf("idle: %d posts before the heartbeat, want 4", poster.count())
	}
	p.tick(ctx, at(45)) // 30 s after the empty snapshot: heartbeat
	if poster.count() != 5 {
		t.Fatalf("heartbeat: %d posts, want 5", poster.count())
	}

	sessions = []Session{session("b")}
	p.tick(ctx, at(50)) // a session appears mid-idle: published on the next tick
	if poster.count() != 6 || len(poster.last(t).Sessions) != 1 {
		t.Fatalf("new session: %d posts, want it published at once", poster.count())
	}
}

// An empty snapshot that failed to land is retried on the next tick: the web
// must not be left showing sessions that already ended.
func TestPublisherRetriesUndeliveredEmpty(t *testing.T) {
	ctx := context.Background()
	sessions := []Session{session("a")}
	poster := &fakePoster{}
	p, _ := pubWith(&sessions, poster)
	p.tick(ctx, at(0))

	sessions = nil
	poster.setErr(errors.New("connection refused"))
	p.tick(ctx, at(5))
	poster.setErr(nil)
	p.tick(ctx, at(10))
	if poster.count() != 3 {
		t.Fatalf("%d posts, want the failed empty snapshot retried", poster.count())
	}
	p.tick(ctx, at(15))
	if poster.count() != 3 {
		t.Fatalf("%d posts, want silence after the empty snapshot landed", poster.count())
	}
}

// A web that predates the endpoint (404/405) is left alone for 10 minutes, and
// says so in the log ONCE however many pauses follow.
func TestPublisherUnsupportedPausesTenMinutes(t *testing.T) {
	ctx := context.Background()
	sessions := []Session{session("a")}
	poster := &fakePoster{err: fmt.Errorf("%w (HTTP 404)", ErrPublishUnsupported)}
	p, sink := pubWith(&sessions, poster)

	if wait := p.tick(ctx, at(0)); wait != PublishUnsupportedPause {
		t.Fatalf("after a 404 the publisher waits %s, want %s", wait, PublishUnsupportedPause)
	}
	if wait := p.tick(ctx, at(60)); wait != PublishUnsupportedPause-time.Minute || poster.count() != 1 {
		t.Fatalf("inside the pause: wait=%s posts=%d, want the rest of the pause and no post", wait, poster.count())
	}
	p.tick(ctx, at(600)) // pause over: try again, still old
	if poster.count() != 2 {
		t.Fatalf("%d posts, want a retry after 10 min", poster.count())
	}
	if sink.count() != 1 {
		t.Fatalf("logged %d lines over two 404 pauses, want 1: %q", sink.count(), sink.lines)
	}
}

// 400/413: logged once, then retried with a growing backoff, reset on success.
func TestPublisherRejectedBacksOff(t *testing.T) {
	ctx := context.Background()
	sessions := []Session{session("a")}
	poster := &fakePoster{err: fmt.Errorf("%w: API error 413", ErrPublishRejected)}
	p, sink := pubWith(&sessions, poster)

	w1 := p.tick(ctx, at(0))
	w2 := p.tick(ctx, at(0).Add(w1))
	if w1 < PublishActiveInterval || w2 <= w1 {
		t.Fatalf("backoff %s then %s, want it above the tick and growing", w1, w2)
	}
	if sink.count() != 1 {
		t.Fatalf("logged %d lines for two rejections, want 1: %q", sink.count(), sink.lines)
	}
	poster.setErr(nil)
	if wait := p.tick(ctx, at(0).Add(w1+w2)); wait != PublishActiveInterval {
		t.Fatalf("after a success the publisher waits %s, want %s", wait, PublishActiveInterval)
	}
}

// Any other failure (network, 5xx, 401/403) drops the snapshot — the next tick
// replaces it, nothing queues — and logs once per failure streak.
func TestPublisherTransientFailureLogsOnce(t *testing.T) {
	ctx := context.Background()
	sessions := []Session{session("a")}
	poster := &fakePoster{err: errors.New("API error 503")}
	p, sink := pubWith(&sessions, poster)

	for i := 0; i < 5; i++ {
		if wait := p.tick(ctx, at(i*5)); wait != PublishActiveInterval {
			t.Fatalf("failing tick waits %s, want %s", wait, PublishActiveInterval)
		}
	}
	if sink.count() != 1 {
		t.Fatalf("logged %d lines over 5 failures, want 1: %q", sink.count(), sink.lines)
	}
	poster.setErr(nil)
	p.tick(ctx, at(25))
	if sink.count() != 2 || !strings.Contains(sink.lines[1], "resumed") {
		t.Fatalf("recovery logged %q, want one 'resumed' line", sink.lines)
	}
}

// A server that never answers costs the publisher one request timeout, not the
// daemon anything: the post runs on the publisher's own goroutine.
func TestPublisherSlowServerTimesOut(t *testing.T) {
	sessions := []Session{session("a")}
	poster := &fakePoster{block: true}
	p, _ := pubWith(&sessions, poster)
	p.Timeout = 20 * time.Millisecond

	start := time.Now()
	p.tick(context.Background(), at(0))
	if took := time.Since(start); took > time.Second {
		t.Fatalf("a hung server held the tick for %s, want ~%s", took, p.Timeout)
	}
}

// Stop cancels an in-flight post and returns only once the goroutine is gone.
func TestPublisherStopIsClean(t *testing.T) {
	sessions := []Session{session("a")}
	poster := &fakePoster{block: true}
	p, _ := pubWith(&sessions, poster)
	p.Timeout = time.Hour // only Stop can end the post

	stop := p.Start(context.Background())
	deadline := time.Now().Add(2 * time.Second)
	for poster.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if poster.count() == 0 {
		t.Fatal("the publisher never posted")
	}

	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return: the publisher goroutine leaked")
	}
	stop() // idempotent
}

// Cancelling the daemon context ends the loop too.
func TestPublisherEndsWithContext(t *testing.T) {
	sessions := []Session{}
	p, _ := pubWith(&sessions, &fakePoster{})
	ctx, cancel := context.WithCancel(context.Background())
	stop := p.Start(ctx)
	cancel()

	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the publisher outlived its context")
	}
}

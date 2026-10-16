package metrics

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"
)

// Publishing the session view to the web (POST /api/internal/agent/sessions).
//
// One goroutine, one request at a time, nothing queued: a snapshot that fails to
// land is dropped and the next tick's fresher one replaces it. The post runs on
// the publisher's own goroutine with its own timeout, so a slow or dead server
// costs the publisher a tick and never the daemon or a download.
const (
	// PublishActiveInterval is the local poll tick, and the post cadence while
	// at least one session exists.
	PublishActiveInterval = 5 * time.Second
	// PublishIdleInterval is the heartbeat while the view is empty: one empty
	// snapshot when it empties, then one per interval so the web can tell an idle
	// agent from one that stopped publishing.
	PublishIdleInterval = 30 * time.Second
	// PublishUnsupportedPause is how long a web that predates the endpoint
	// (404/405) is left alone before asking again.
	PublishUnsupportedPause = 10 * time.Minute
	// PublishTimeout bounds one request.
	PublishTimeout = 5 * time.Second
	// PublishStopTimeout bounds the final empty snapshot sent on stop: the most
	// a slow or dead server can add to the daemon's shutdown.
	PublishStopTimeout = 2 * time.Second

	publishBackoffMin = 30 * time.Second
	publishBackoffMax = 10 * time.Minute
)

var (
	// ErrPublishUnsupported is what a Poster returns when the web has no such
	// endpoint (HTTP 404/405 from the app itself).
	ErrPublishUnsupported = errors.New("the server does not accept session snapshots")
	// ErrPublishRejected is what a Poster returns when the web refused the body
	// (HTTP 400/413).
	ErrPublishRejected = errors.New("the server rejected the session snapshot")
)

// Poster delivers one encoded snapshot (the JSON of GET /sessions) to the web.
// It must honour ctx: the publisher's timeout and shutdown both arrive that way.
type Poster interface {
	PostSessions(ctx context.Context, snapshot []byte) error
}

// Clock is the publisher's time source; tests replace it.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Publisher sends the hub's snapshot to the web on the cadence above. Set the
// exported fields before Start; the rest is owned by the publisher goroutine.
type Publisher struct {
	poll   func(time.Time) Snapshot
	poster Poster

	Clock       Clock
	Logf        func(format string, args ...any)
	Timeout     time.Duration
	StopTimeout time.Duration

	latest    Snapshot      // the last one polled: the base of the final empty one
	delivered bool          // a snapshot has landed since start
	lastEmpty bool          // ...and the latest one to land was empty
	lastAt    time.Time     // when it landed
	holdUntil time.Time     // no post before this (404 pause, rejection backoff)
	backoff   time.Duration // current rejection backoff
	failClass string        // failure streak in progress ("" = healthy); logged once
}

// NewPublisher builds a publisher over poll (Hub.Poll in the daemon).
func NewPublisher(poll func(time.Time) Snapshot, poster Poster) *Publisher {
	return &Publisher{
		poll: poll, poster: poster, Clock: realClock{}, Logf: log.Printf,
		Timeout: PublishTimeout, StopTimeout: PublishStopTimeout,
	}
}

// Start runs the publisher until ctx ends or stop is called. stop cancels an
// in-flight request, waits for the goroutine to exit, then sends the final
// empty snapshot (see farewell) — even when ctx has already ended, which is how
// a daemon shutdown reaches it. It holds the caller at most StopTimeout longer
// and is idempotent.
func (p *Publisher) Start(ctx context.Context) (stop func()) {
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.run(runCtx)
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
			p.farewell(context.WithoutCancel(ctx))
		})
	}
}

// farewell posts ONE empty snapshot so the web drops this agent's sessions now
// instead of when they go stale. Best effort: skipped when nothing has landed,
// when the web already holds an empty view, or when it does not take snapshots
// at all (404/405); bounded by StopTimeout even if the Poster ignores its
// context. Runs after the publisher goroutine has exited, so it owns p.
func (p *Publisher) farewell(ctx context.Context) {
	if !p.delivered || p.lastEmpty || p.failClass == "unsupported" {
		return
	}
	body, err := encodeForPublish(emptied(p.latest, p.Clock.Now()))
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, p.StopTimeout)
	defer cancel()
	res := make(chan error, 1)
	go func() { res <- p.poster.PostSessions(ctx, body) }()
	select {
	case err = <-res:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		p.Logf("[sessions] could not clear the web's session view on stop (%v)", err)
	}
}

// emptied is s with no session in it: nothing active, stalled or moving, while
// the agent and the monotonic totals stay what they were.
func emptied(s Snapshot, now time.Time) Snapshot {
	if !s.GeneratedAt.IsZero() && now.After(s.GeneratedAt) {
		s.Agent.UptimeSec += int64(now.Sub(s.GeneratedAt).Seconds())
	}
	s.GeneratedAt = now.UTC()
	s.Sessions = []Session{}
	s.Summary.Active = make(map[Kind]int, len(Kinds))
	s.Summary.RateBps = make(map[Kind]int64, len(Kinds))
	for _, k := range Kinds {
		s.Summary.Active[k], s.Summary.RateBps[k] = 0, 0
	}
	s.Summary.Stalled = 0
	return s
}

func (p *Publisher) run(ctx context.Context) {
	for {
		wait := p.tick(ctx, p.Clock.Now())
		select {
		case <-ctx.Done():
			return
		case <-p.Clock.After(wait):
		}
	}
}

// tick samples, posts when due, and returns how long to wait for the next one.
func (p *Publisher) tick(ctx context.Context, now time.Time) time.Duration {
	if now.Before(p.holdUntil) {
		return p.holdUntil.Sub(now)
	}
	snap := p.poll(now)
	p.latest = snap
	empty := len(snap.Sessions) == 0
	if !p.due(empty, now) {
		return PublishActiveInterval
	}
	body, err := encodeForPublish(snap)
	if err == nil {
		err = p.post(ctx, body)
	}
	if err != nil {
		if ctx.Err() != nil {
			return PublishActiveInterval // shutting down: not a failure to report
		}
		return p.failed(err, now)
	}
	p.succeeded(empty, now)
	return PublishActiveInterval
}

// due: every tick with sessions; an empty view only until one empty snapshot
// has landed, then once per heartbeat.
func (p *Publisher) due(empty bool, now time.Time) bool {
	if !empty || !p.delivered || !p.lastEmpty {
		return true
	}
	return now.Sub(p.lastAt) >= PublishIdleInterval
}

func (p *Publisher) post(ctx context.Context, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	return p.poster.PostSessions(ctx, body)
}

func (p *Publisher) succeeded(empty bool, now time.Time) {
	if p.failClass != "" {
		p.Logf("[sessions] publishing to the web resumed")
	}
	p.delivered, p.lastEmpty, p.lastAt = true, empty, now
	p.failClass, p.backoff = "", 0
}

// failed records a failure and returns the wait before the next attempt. Each
// streak is logged once, when it starts: a web without the endpoint, or a
// network that is down, must not write a line every 5 s.
func (p *Publisher) failed(err error, now time.Time) time.Duration {
	class, wait := "error", PublishActiveInterval
	switch {
	case errors.Is(err, ErrPublishUnsupported):
		class, wait = "unsupported", PublishUnsupportedPause
	case errors.Is(err, ErrPublishRejected):
		p.backoff = min(max(2*p.backoff, publishBackoffMin), publishBackoffMax)
		class, wait = "rejected", p.backoff
	}
	if class != p.failClass {
		p.failClass = class
		p.logFailure(class, err)
	}
	if wait > PublishActiveInterval {
		p.holdUntil = now.Add(wait)
	}
	return wait
}

func (p *Publisher) logFailure(class string, err error) {
	switch class {
	case "unsupported":
		p.Logf("[sessions] the server does not accept session snapshots yet (%v) - asking again every %s", err, PublishUnsupportedPause)
	case "rejected":
		p.Logf("[sessions] the server rejected the session snapshot (%v) - backing off", err)
	default:
		p.Logf("[sessions] could not publish to the web (%v) - retrying every %s", err, PublishActiveInterval)
	}
}

package stream

import (
	"errors"
	"log"
	"slices"
	"sync"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/yenc"
)

// The article a consumer is waiting on — where a seek lands, or where playback
// starts — is fetched once and, if it runs well past the fetches before it, again
// on another connection, serving whichever completes first. The provider serves
// the same article at very different speeds from one BODY to the next: across a
// session the median fetch took ~0.5 s while one in ten took over 1.4 s and one in
// twenty over 2 s, and each of those is a player frozen on a seek. A second fetch
// started once the first is clearly slow usually beats it.
//
// The slower fetch is not interrupted: cutting a BODY mid-transfer drops its
// connection, and redialling after every seek stalled other fetches for seconds.
// It finishes in the background and its buffer is recycled. Read-ahead never
// hedges (nobody is waiting on it yet), nor does a reader on a pool too small to
// run both fetches side by side, or one capped by a fetch budget (no player).

var (
	// HedgeLatencyMultiple sets how far past the median of recent fetches the
	// consumer's fetch may run before a second one races it; 0 disables hedging.
	HedgeLatencyMultiple = 2
	// HedgeMinDelay and HedgeMaxDelay bound that wait: a fast link must not
	// hedge every fetch, and a slow one must still hedge.
	HedgeMinDelay = 400 * time.Millisecond
	HedgeMaxDelay = 3 * time.Second
)

const (
	// latencySamples is how many recent fetch durations the median is taken over,
	// minLatencySamples how many are needed before hedging at all.
	latencySamples    = 32
	minLatencySamples = 8
	// latencyMaxAge ignores samples from an earlier session, whose link says
	// nothing about this one. The ring is shared by every source of the cache,
	// which in practice all use the same provider.
	latencyMaxAge = 5 * time.Minute
)

type latencySample struct {
	d  time.Duration
	at time.Time
}

// latencyRing keeps the durations of the most recent successful article fetches.
type latencyRing struct {
	mu      sync.Mutex
	samples [latencySamples]latencySample
	next    int // slot the next sample overwrites
}

// note records one successful fetch's duration.
func (l *latencyRing) note(d time.Duration) {
	l.mu.Lock()
	l.samples[l.next] = latencySample{d: d, at: time.Now()}
	l.next = (l.next + 1) % latencySamples
	l.mu.Unlock()
}

// hedgeDelay is how long the consumer's fetch runs before a second one starts, or
// false when hedging is off or too few recent fetches have been seen to judge.
func (l *latencyRing) hedgeDelay() (time.Duration, bool) {
	if HedgeLatencyMultiple <= 0 {
		return 0, false
	}
	cutoff := time.Now().Add(-latencyMaxAge)
	recent := make([]time.Duration, 0, latencySamples)
	l.mu.Lock()
	for _, s := range l.samples {
		if s.at.After(cutoff) {
			recent = append(recent, s.d)
		}
	}
	l.mu.Unlock()
	if len(recent) < minLatencySamples {
		return 0, false
	}
	slices.Sort(recent)
	delay := time.Duration(HedgeLatencyMultiple) * recent[len(recent)/2]
	return min(max(delay, HedgeMinDelay), HedgeMaxDelay), true
}

// hedgeDelay is the latency ring's delay for this reader, false for a reader that
// must not hedge: on a small pool the second fetch would only queue behind the
// first and then pull an article nobody reads, and a budgeted reader has no
// player waiting.
func (r *Reader) hedgeDelay() (time.Duration, bool) {
	if r.budget != nil || r.smallPool() {
		return 0, false
	}
	return r.cache.c.latency.hedgeDelay()
}

type fetchResult struct {
	part *yenc.Part
	err  error
}

// fetchHedged is fetchDecodeRetry for an article the consumer is waiting on: past
// hedgeDelay a second fetch races the first, and the first success is served.
func (r *Reader) fetchHedged(messageID string, estBytes int64) (*yenc.Part, error) {
	delay, ok := r.hedgeDelay()
	if !ok {
		return r.fetchDecodeRetry(messageID, estBytes)
	}
	results := make(chan fetchResult, 2)
	r.raceFetch(messageID, estBytes, results)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case res := <-results:
		return res.part, res.err
	case <-r.ctx.Done():
		r.discardFetches(results, 1)
		return nil, r.ctx.Err()
	case <-timer.C:
	}
	log.Printf("[usenet-stream] article %s still fetching after %v, racing a second fetch", messageID, delay)
	r.raceFetch(messageID, estBytes, results)
	return r.firstServable(results)
}

// firstServable returns the first of two racing fetches that succeeded or found
// the article gone, leaving the other to finish in the background. When both fail
// otherwise, the error kept is one about the article rather than about the fetch
// that ran into it, so the dead-article memo and a waiter's retry see the truth.
func (r *Reader) firstServable(results <-chan fetchResult) (*yenc.Part, error) {
	first := <-results
	if first.err == nil || articleGone(first.err) {
		r.discardFetches(results, 1)
		return first.part, first.err
	}
	second := <-results
	if second.err == nil || articleGone(second.err) || leaderSpecific(first.err) {
		return second.part, second.err
	}
	return nil, first.err
}

// articleGone reports a final verdict on the article itself: the server said it
// does not exist. A stall is not one — the other fetch may still get it.
func articleGone(err error) bool {
	var u *ArticleUnavailableError
	return errors.As(err, &u) && !u.Stalled
}

// raceFetch runs one fetch of the article in the background, delivering to out.
func (r *Reader) raceFetch(messageID string, estBytes int64, out chan<- fetchResult) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		part, err := r.fetchDecodeRetry(messageID, estBytes)
		out <- fetchResult{part: part, err: err}
	}()
}

// discardFetches waits in the background for n fetches nobody will serve and
// releases what they decoded.
func (r *Reader) discardFetches(results <-chan fetchResult, n int) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		for range n {
			if res := <-results; res.part != nil {
				r.cache.c.releasePart(res.part)
			}
		}
	}()
}

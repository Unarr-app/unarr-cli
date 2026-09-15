package stream

import (
	"fmt"
	"sync"

	"github.com/Unarr-app/unarr-cli/internal/usenet/yenc"
)

// A seek lands inside an article, usually far from its start, and used to wait
// for the whole article — a body of several MB over one connection, half a second
// to two — before its first byte. Most of that wait is for bytes behind the one
// the player asked for. yEnc decodes line by line, over the front of the buffer
// the body is read into, and =ypart places the article in the file within its
// first hundred bytes. So a fetch publishes the front it has decoded as the body
// arrives, and a reader whose position falls in it is served at once, waiting
// only for the bytes it needs.
//
// The published bytes live in the fetch's buffer, which is recycled if the body
// fails. Readers copy them only under the arrival's lock, and the fetch withdraws
// them under that lock before it lets the buffer go. Once the fetch completes the
// reader takes the article itself, as a waiter on its flight.
//
// Served bytes are not yet verified: the article's CRC covers the whole body. A
// fetch whose served front then fails — a CRC mismatch, a read that breaks and is
// retried, an error — taints the arrival, and a reader that was served from it
// fails its read instead of carrying on past bytes that may be wrong. A front
// withdrawn before anyone was served from it leaves the arrival open to the next
// fetch of the article: a retry, or a hedge racing the first.

// arrivalPublishStep is how many newly decoded bytes a fetch collects before it
// publishes them: a player reads 32 KiB at a time, and publishing on every line
// would take the lock tens of thousands of times per article.
const arrivalPublishStep = 64 << 10

// errServedArticleFailed fails a read that was served bytes of an article whose
// fetch then failed verification.
var errServedArticleFailed = fmt.Errorf("usenet reader: article failed after part of it was served")

// arrival is the published front of a flight's article while it downloads.
type arrival struct {
	mu      sync.Mutex
	owner   *bodyFeed     // the fetch publishing; another racing it (a hedge) does not
	data    []byte        // decoded bytes so far, in owner's buffer
	start   int64         // file offset of data[0]
	end     int64         // file offset the article ends at; 0 when unknown
	served  bool          // a reader copied from owner's front
	closed  bool          // withdrawn after serving: nothing more will be published
	tainted bool          // the fetch that served failed afterwards
	changed chan struct{} // closed by the next publish or withdraw
}

// publish makes data the published front, unless another fetch owns the arrival
// or it was closed.
func (a *arrival) publish(f *bodyFeed, data []byte, start, end int64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || (a.owner != nil && a.owner != f) {
		return false
	}
	a.owner, a.data, a.start, a.end = f, data, start, end
	a.wakeLocked()
	return true
}

// withdraw takes back what f published. Once readers were served from it the
// arrival closes, so no other fetch's bytes are mixed in; otherwise it is open
// to the next fetch.
func (a *arrival) withdraw(f *bodyFeed) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.owner != f {
		return
	}
	a.data = nil
	if a.served {
		a.closed = true
	} else {
		a.owner = nil
	}
	a.wakeLocked()
}

// taint records that f failed after readers were served from its front.
func (a *arrival) taint(f *bodyFeed) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.owner == f && a.served {
		a.tainted = true
	}
}

func (a *arrival) isTainted() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tainted
}

func (a *arrival) wakeLocked() {
	if a.changed != nil {
		close(a.changed)
		a.changed = nil
	}
}

type arrivalState int

const (
	arrivalServed  arrivalState = iota
	arrivalPending              // not there yet: wait for changed
	arrivalClosed               // nothing more will be published: wait for the fetch
	arrivalOutside              // the article does not cover the position
)

// copyAt copies the published bytes at file offset pos into p. When they are
// not there yet it returns the channel closed by the next change.
func (a *arrival) copyAt(pos int64, p []byte) (int, arrivalState, <-chan struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.closed:
		return 0, arrivalClosed, nil
	case a.owner == nil:
	case pos < a.start || (a.end > 0 && pos >= a.end):
		return 0, arrivalOutside, nil
	case pos-a.start < int64(len(a.data)):
		a.served = true
		return copy(p, a.data[pos-a.start:]), arrivalServed, nil
	}
	if a.changed == nil {
		a.changed = make(chan struct{})
	}
	return 0, arrivalPending, a.changed
}

// bodyFeed decodes one fetch's body as it arrives (nntp.BodyProgress) and
// publishes the decoded front to the arrival of the flight it fetches for.
type bodyFeed struct {
	dec  yenc.InPlaceDecoder
	to   *arrival // the arrival published to; nil: nobody reads this body early
	live bool     // still publishing
	fed  int      // body bytes last fed to the decoder
	sent int      // decoded bytes last published
}

// arrivalFeedStep is how many body bytes arrive between decodes while a feed
// publishes; a feed that does not leaves the whole body to Finish.
const arrivalFeedStep = 16 << 10

func newBodyFeed(to *arrival) *bodyFeed { return &bodyFeed{to: to, live: to != nil} }

// decoder is the feed's decoder, nil for no feed (a body decoded into a copy).
func (f *bodyFeed) decoder() *yenc.InPlaceDecoder {
	if f == nil {
		return nil
	}
	return &f.dec
}

// Line decodes the body received so far and publishes every arrivalPublishStep.
// A body that outgrows its buffer moves to a larger array, published from the
// next step on; the old buffer still holds what was published until stop.
func (f *bodyFeed) Line(body []byte) {
	if !f.live || len(body)-f.fed < arrivalFeedStep {
		return
	}
	f.fed = len(body)
	f.dec.Feed(body)
	start, end, n, ok := f.dec.Progress()
	if !ok || n-f.sent < arrivalPublishStep {
		return
	}
	if !f.to.publish(f, body[:n], start, end) {
		f.stop()
		return
	}
	f.sent = n
}

// Restart voids what was decoded: the body is read again over the same buffer,
// so bytes served from the broken read are no longer the ones verified.
func (f *bodyFeed) Restart() {
	f.fail()
	f.dec, f.fed, f.sent = yenc.InPlaceDecoder{}, 0, 0
}

// stop withdraws what the feed published and publishes no more. It must run
// before the feed's buffer is recycled.
func (f *bodyFeed) stop() {
	if f.live {
		f.live = false
		f.to.withdraw(f)
	}
}

// fail stops the feed and taints its arrival if readers were served from it.
func (f *bodyFeed) fail() {
	if f == nil {
		return
	}
	f.stop()
	if f.to != nil {
		f.to.taint(f)
	}
}

// holds reports whether the reader's current article covers pos.
func (r *Reader) holds(pos int64) bool {
	return r.cur != nil && pos >= r.cur.Begin-1 && pos < r.cur.Begin-1+int64(len(r.cur.Data))
}

// readArriving serves p from the article covering the read position while that
// article downloads, as soon as its published front reaches the position. ok is
// false when the read is left to articleForOffset: the article became the
// current one (cached, or its fetch completed), or the position turned out to be
// in another article than the index guessed.
func (r *Reader) readArriving(p []byte) (n int, ok bool, err error) {
	if r.holds(r.pos) {
		r.dropArriving()
		return 0, false, nil
	}
	seg, _, _, found := r.ix.Locate(r.pos)
	if !found {
		return 0, false, nil
	}
	s := r.ix.Segment(seg)
	if s.MessageID == r.curID {
		r.dropArriving()
		return 0, false, nil
	}
	fl := r.awaitArticle(s.MessageID, s.Bytes)
	if fl == nil {
		return 0, false, nil
	}
	for {
		select {
		case <-fl.done:
			return 0, false, r.takeArrived()
		default:
		}
		n, state, changed := fl.arrival.copyAt(r.pos, p)
		switch state {
		case arrivalServed:
			r.arrivalWaited, r.arrivalServed = true, true
			return n, true, nil
		case arrivalOutside:
			r.dropArriving()
			return 0, false, nil
		case arrivalPending, arrivalClosed:
		}
		r.arrivalWaited = true
		select {
		case <-changed: // nil when closed: wait for the fetch instead
		case <-fl.done:
		case <-r.ctx.Done():
			return 0, false, r.ctx.Err()
		}
	}
}

// awaitArticle makes the reader a waiter on the article id: it becomes the
// current article at once when cached (nil is returned), or the reader waits on
// the flight fetching it, starting that fetch in the background when there is
// none.
func (r *Reader) awaitArticle(id string, estBytes int64) *flight {
	if r.arrivingID == id {
		return r.arriving
	}
	r.dropArriving()
	if r.ctx.Err() != nil {
		return nil
	}
	part, fl, leader := r.cache.begin(id, true)
	if part != nil {
		r.cache.c.releasePart(r.cur)
		r.cur, r.curID, r.waited = part, id, false
		return nil
	}
	if leader {
		r.cache.wait(fl)
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			part, err := r.fetchHedged(id, estBytes, fl)
			r.cache.finish(id, fl, part, err)
			r.cache.c.releasePart(part) // the cache and waiters hold their own references
		}()
	}
	r.arriving, r.arrivingID = fl, id
	r.arrivalWaited, r.arrivalServed = false, false
	return fl
}

// takeArrived makes the article whose flight completed the current one. An error
// about the article is the read's, as for any waiter (see load); one that
// belonged to the fetching reader is left to articleForOffset to fetch again.
// So is nothing, when the reader was served bytes the fetch then failed to
// verify: the response is cut rather than continued past them.
func (r *Reader) takeArrived() error {
	fl, id, served := r.arriving, r.arrivingID, r.arrivalServed
	r.arriving, r.arrivingID = nil, ""
	if served && fl.arrival.isTainted() {
		if fl.err == nil {
			r.cache.c.releasePart(fl.part)
		}
		return fmt.Errorf("%w: %s", errServedArticleFailed, id)
	}
	if fl.err != nil {
		if fl.retry {
			return nil
		}
		return fl.err
	}
	r.cache.c.releasePart(r.cur)
	r.cur, r.curID, r.waited = fl.part, id, r.arrivalWaited
	return nil
}

// dropArriving stops waiting on the article the reader was waiting on.
func (r *Reader) dropArriving() {
	if r.arriving != nil {
		r.cache.abandon(r.arriving)
		r.arriving, r.arrivingID = nil, ""
	}
}

// wait counts one more waiter on fl, taking a reference to its part when it finishes.
func (s *CacheScope) wait(fl *flight) {
	s.c.mu.Lock()
	fl.waiters++
	s.c.mu.Unlock()
}

// waited reports whether any reader still waits on fl.
func (s *CacheScope) waited(fl *flight) bool {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	return fl.waiters > 0
}

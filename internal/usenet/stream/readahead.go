package stream

import (
	"log"
	"sync"
	"time"
)

// readaheadMissLatency is how long a load may take and still count as served
// from the cache. Longer means the consumer waited for the network: it outran
// the read-ahead.
const readaheadMissLatency = 5 * time.Millisecond

// readaheadPoolMultiple sizes the widest window against the connection pool.
// More articles than connections keeps every connection busy: the moment one
// finishes, the next article is already queued for it. A window no wider than
// the pool slides one article per article consumed, so connections that finished
// early sit idle while the consumer waits on the front article — and over a
// 50+ ms round trip an idle connection restarts TCP slow start.
const readaheadPoolMultiple = 2

// prefetchQueue is a reader's pending read-ahead: segments wanted but not yet
// started, fetched by at most pool-1 workers that always take the lowest pending
// segment ahead of the cursor.
type prefetchQueue struct {
	mu      sync.Mutex
	pending map[int]pendingArticle
	workers int
	// front is the article the consumer is on: anything at or behind it is no
	// longer read-ahead (a seek moved away) and is dropped instead of fetched.
	front int
	// dropped is set when a worker discarded queued articles because the reader
	// had gone idle, so the next trigger queues them again.
	dropped bool

	// lastFront/lastK are the front and window last queued, read and written only
	// by the consumer's goroutine (not under mu).
	lastFront, lastK int
}

type pendingArticle struct {
	id    string
	bytes int64
}

// triggerReadahead queues the articles following fromSeg so the next sequential
// reads hit the cache. The window is an article count (readaheadK, widened by
// rampReadahead) clamped by a BYTE budget converted with the size of the article
// just served, and by ReadaheadMaxArticles — so the cushion carried ahead of the
// cursor stays bounded in the unit we are billed in, whatever the poster's
// article size. Message-ids AND size estimates are resolved here on the read
// goroutine; the workers never touch the index.
func (r *Reader) triggerReadahead(fromSeg, articleBytes int) {
	if r.readaheadK <= 0 {
		return
	}
	if !r.raCounted {
		r.cache.c.countReadahead(1)
		r.raCounted = true
	}
	r.rampReadahead(fromSeg)
	// The ~23 Reads inside one article would otherwise rebuild the same queue (and
	// take the cache lock) each time. The window only changes when the consumer
	// moves on, so it is recomputed then, after a seek, or when queued articles
	// were dropped while the reader was idle.
	if fromSeg == r.pq.lastFront && !r.pq.takeDropped() {
		return
	}
	r.pq.lastFront = fromSeg
	k := r.readaheadWindow(articleBytes)
	if k <= 0 {
		return
	}
	r.pq.lastK = k
	n := r.ix.SegmentCount()
	want := make(map[int]pendingArticle, k)
	for j := fromSeg + 1; j <= fromSeg+k && j < n; j++ {
		seg := r.ix.Segment(j)
		want[j] = pendingArticle{id: seg.MessageID, bytes: seg.Bytes}
	}
	r.enqueue(fromSeg, want)
}

// arrivalReadahead is how many articles past the landing one start with it
// (readaheadOnArrival). They are billed even if the request is abandoned before
// its first byte, so the rest of the window still waits for that byte.
const arrivalReadahead = 2

// readaheadOnArrival starts the articles after the one pos lands on together with
// it, when the consumer has just arrived there: a new reader or a seek. Queued
// only after the first byte, the next article starts once the landing one is
// complete, so a read that runs past it — a player refilling its buffer after a
// seek, or starting playback — waits for two articles one after the other. The
// full window is still queued after the first byte, and sequential reads keep
// queuing there, where the window learns whether the consumer waited.
func (r *Reader) readaheadOnArrival(pos int64) {
	seg, ok := r.arrivedAt(pos)
	if !ok {
		return
	}
	r.rampReadahead(seg) // first: a seek narrows the window before it is sized
	bytes := r.ix.Segment(seg).Bytes
	if bytes <= 0 {
		return // no size to budget with: leave it to the queue after the first byte
	}
	k := min(arrivalReadahead, r.readaheadWindow(int(bytes)))
	want := make(map[int]pendingArticle, k)
	for j := seg + 1; j <= seg+k && j < r.ix.SegmentCount(); j++ {
		s := r.ix.Segment(j)
		want[j] = pendingArticle{id: s.MessageID, bytes: s.Bytes}
	}
	r.enqueue(seg, want)
}

// arrivedAt returns the segment a Read at pos arrives in when that is a new
// place worth reading ahead from: not the article already held or the next one
// in order, not on a pool too small to spare a connection from the landing
// article, and not while the map around pos is still an estimate — it can be
// several articles off, and they would be billed for nothing.
func (r *Reader) arrivedAt(pos int64) (int, bool) {
	if r.readaheadK <= 0 || (r.cur != nil && pos >= r.cur.Begin-1 && pos < r.cur.Begin-1+int64(len(r.cur.Data))) {
		return 0, false
	}
	if r.smallPool() {
		return 0, false
	}
	seg, exact, ok := r.ix.LocateExact(pos)
	if !ok || !exact || seg == r.lastSeg || (r.lastSeg >= 0 && seg == r.lastSeg+1) {
		return 0, false
	}
	return seg, true
}

// smallPool reports a connection pool of two or fewer: a second fetch started
// for the article a consumer waits on would only queue behind it.
func (r *Reader) smallPool() bool {
	h, ok := r.fetcher.(concurrencyHinter)
	return ok && h.MaxConcurrency() <= 2
}

// rampReadahead widens the window while the consumer reads straight through AND
// keeps catching up with it, and narrows it back on a seek. A consumer reading at
// the video's bitrate never waits, so it stays on the base window and pays for no
// more cushion than before; ffmpeg, a copy or a player filling its buffer widen it
// until the pool is kept busy. Widening waits for a run of readaheadK articles
// read in order, so a short range (a player's seek probe, a scrub) does not pull
// a wide window past its end.
func (r *Reader) rampReadahead(fromSeg int) {
	switch {
	case fromSeg == r.lastSeg:
		return // another Read inside the same article
	case fromSeg == r.lastSeg+1:
		r.seqRun++
		r.scaleReadahead()
	default:
		// A seek: the old window's articles are not what comes next.
		r.raScale, r.seqRun, r.calmRun = 1, 0, 0
	}
	r.lastSeg = fromSeg
}

// scaleReadahead doubles the window when the consumer waited for the article it
// just moved on from, and halves it once a whole window's worth of articles went
// by without a wait: the consumer is back at the video's bitrate (a player whose
// buffer filled), so the extra cushion — and the cache boost paying for it — no
// longer earns anything.
func (r *Reader) scaleReadahead() {
	if !r.waited {
		r.calmRun++
		if r.raScale > 1 && r.calmRun >= r.readaheadK*r.raScale {
			r.raScale /= 2
			r.calmRun = 0
		}
		return
	}
	r.calmRun = 0
	if r.readaheadK > 0 && r.seqRun >= r.readaheadK && r.readaheadK*r.raScale < min(r.rampCeiling(), ReadaheadMaxArticles) {
		r.raScale *= 2
	}
}

// rampCeiling is the widest window rampReadahead may grow to, never below the
// base window.
func (r *Reader) rampCeiling() int {
	if h, ok := r.fetcher.(concurrencyHinter); ok {
		return max(r.readaheadK, readaheadPoolMultiple*h.MaxConcurrency())
	}
	return r.readaheadK
}

// prefetchWorkers is how many read-ahead goroutines one reader may run: as many
// as the shared gate can ever let fetch.
func (r *Reader) prefetchWorkers() int { return r.readaheadSlots() }

// readaheadSlots is how many read-ahead fetches may run at once across every
// reader of the cache: the pool less ReadaheadReserveConns, which stay free for
// the article a consumer is actually waiting on (a seek, a new request), capped
// by ReadaheadMaxSlots when set.
func (r *Reader) readaheadSlots() int {
	n := max(1, r.readaheadK)
	if h, ok := r.fetcher.(concurrencyHinter); ok {
		n = max(1, h.MaxConcurrency()-ReadaheadReserveConns)
	}
	if ReadaheadMaxSlots > 0 {
		n = min(n, ReadaheadMaxSlots)
	}
	return n
}

// readaheadWindow returns how many articles may be prefetched: the article
// window, capped by ReadaheadMaxArticles and by the byte budget divided by the
// observed article size. A zero/unknown article size falls back to the count.
func (r *Reader) readaheadWindow(articleBytes int) int {
	k := r.readaheadK
	if k <= 0 {
		return 0
	}
	if r.raScale > 1 {
		k = min(k*r.raScale, r.rampCeiling())
	}
	k = min(k, ReadaheadMaxArticles)
	if articleBytes > 0 {
		if budget := r.boostedBudget(int64(k) * int64(articleBytes)); budget > 0 {
			k = min(k, int(budget/int64(articleBytes)))
		}
	}
	return k
}

// boostedBudget is readaheadBudget plus the cache boost this reader holds. A
// widened window (raScale > 1: a consumer that outran the read-ahead) that needs
// more than the budget borrows the rest from the cache, as far as its boost
// ceiling allows; a window back at the base returns what it borrowed.
func (r *Reader) boostedBudget(need int64) int64 {
	budget := r.readaheadBudget()
	if budget <= 0 {
		return budget
	}
	var want int64
	// boostCeiling is fixed before the cache is shared, so it is read unlocked.
	if r.raScale > 1 && r.cache.c.boostCeiling > 0 {
		want = max(0, need-budget)
	}
	return budget + r.setBoost(want)
}

// setBoost resizes the reader's cache boost towards want and returns what it
// holds. While it holds any, a timer returns it once the reader goes idle: a
// paused player must not keep the cache grown, shutting other readers out of
// the ceiling, until it reads again or closes.
func (r *Reader) setBoost(want int64) int64 {
	r.boostMu.Lock()
	defer r.boostMu.Unlock()
	if want != r.raBoost {
		r.raBoost = r.cache.c.resizeBoost(r.raBoost, want)
	}
	if r.raBoost > 0 && r.boostTimer == nil && r.idleWindow > 0 {
		r.boostTimer = time.AfterFunc(r.idleWindow, r.expireBoost)
	}
	return r.raBoost
}

// expireBoost returns the boost of a reader that has gone idle, and checks again
// later on one that is still reading.
func (r *Reader) expireBoost() {
	r.boostMu.Lock()
	defer r.boostMu.Unlock()
	r.boostTimer = nil
	if r.raBoost == 0 {
		return
	}
	if r.readerIdle() {
		r.raBoost = r.cache.c.resizeBoost(r.raBoost, 0)
		return
	}
	r.boostTimer = time.AfterFunc(r.idleWindow, r.expireBoost)
}

// releaseBoost hands the reader's cache boost back and stops its idle timer.
func (r *Reader) releaseBoost() {
	r.boostMu.Lock()
	defer r.boostMu.Unlock()
	if r.boostTimer != nil {
		r.boostTimer.Stop()
		r.boostTimer = nil
	}
	if r.raBoost != 0 {
		r.raBoost = r.cache.c.resizeBoost(r.raBoost, 0)
	}
}

// readaheadBudget is readaheadBytes, bounded by this reader's share of the
// article cache (see ArticleCache.readaheadShare): windows the cache cannot hold
// would evict prefetched articles before they are read and fetch them again.
func (r *Reader) readaheadBudget() int64 {
	budget := r.readaheadBytes
	if share := r.cache.c.readaheadShare(); share > 0 && (budget <= 0 || share < budget) {
		budget = share
	}
	return budget
}

// prefetch queues one segment for read-ahead (see enqueue).
func (r *Reader) prefetch(segIdx int, messageID string, estBytes int64) {
	r.enqueue(segIdx-1, map[int]pendingArticle{segIdx: {id: messageID, bytes: estBytes}})
}

// enqueue replaces the pending read-ahead with want (articles already started
// keep going), moves the front to front, and starts workers for it.
func (r *Reader) enqueue(front int, want map[int]pendingArticle) {
	q := &r.pq
	q.mu.Lock()
	q.front = front
	q.pending = want
	spawn := min(len(want), r.prefetchWorkers()-q.workers)
	q.workers += max(0, spawn)
	q.mu.Unlock()
	for i := 0; i < spawn; i++ {
		r.wg.Add(1)
		go r.prefetchWorker()
	}
}

// dropReadahead discards the queued read-ahead: the consumer seeked away, so
// those articles are no longer what it reads next. Articles already being
// fetched finish. Called by the consumer's goroutine.
func (r *Reader) dropReadahead() {
	r.pq.mu.Lock()
	r.pq.pending = nil
	r.pq.mu.Unlock()
	r.pq.lastFront = -1
}

// takeDropped reports, and clears, whether queued articles were dropped as idle.
func (q *prefetchQueue) takeDropped() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	d := q.dropped
	q.dropped = false
	return d
}

// next hands a worker the lowest pending segment ahead of the front, or reports
// that the queue is empty (and the worker has retired).
func (q *prefetchQueue) next() (int, pendingArticle, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	best := -1
	for seg := range q.pending {
		if seg <= q.front {
			delete(q.pending, seg)
		} else if best < 0 || seg < best {
			best = seg
		}
	}
	if best < 0 {
		q.workers--
		return 0, pendingArticle{}, false
	}
	a := q.pending[best]
	delete(q.pending, best)
	return best, a, true
}

// prefetchWorker fetches queued segments until none is left. A segment another
// reader (or the consumer itself) already has cached or in flight is skipped:
// the claim is taken only when the fetch actually starts, so the consumer never
// waits on an article that is merely queued.
func (r *Reader) prefetchWorker() {
	defer r.wg.Done()
	gate := r.cache.c.prefetchGate(r.readaheadSlots())
	for {
		// The gate is taken before a segment is, so a worker parked on it holds no
		// article that a seek might have made stale.
		select {
		case gate <- struct{}{}:
		case <-r.ctx.Done():
			r.pq.retire()
			return
		}
		more := r.prefetchNext()
		<-gate
		if !more {
			return
		}
	}
}

// prefetchNext fetches the next queued segment, if it is still worth it. It
// returns false once the queue is empty and the worker has retired.
func (r *Reader) prefetchNext() bool {
	segIdx, a, ok := r.pq.next()
	if !ok {
		return false
	}
	if r.ctx.Err() != nil {
		return true
	}
	// A reader that went quiet (player paused, closed, or the HTTP connection
	// cut) must stop pulling articles nobody will read: we are BILLED for them.
	if r.readerIdle() {
		r.pq.mu.Lock()
		r.pq.dropped = true
		r.pq.mu.Unlock()
		return true
	}
	fl, claimed := r.cache.claim(a.id)
	if !claimed {
		return true
	}
	part, err := r.fetchDecodeRetry(a.id, a.bytes)
	r.cache.finish(a.id, fl, part, err)
	r.cache.c.releasePart(part) // the cache and waiters hold their own references
	if err != nil {
		log.Printf("[usenet-stream] read-ahead segment %d abandoned: %v", segIdx, err)
	}
	return true
}

// retire counts a worker out without touching the queue (its reader closed).
func (q *prefetchQueue) retire() {
	q.mu.Lock()
	q.workers--
	q.mu.Unlock()
}

// readerIdle reports whether the consumer has stopped reading for longer than the
// idle window, in which case a queued prefetch is no longer worth its billed
// bytes. A non-positive window disables the check.
func (r *Reader) readerIdle() bool {
	if r.idleWindow <= 0 {
		return false
	}
	r.mu.Lock()
	last := r.lastRead
	r.mu.Unlock()
	return time.Since(last) > r.idleWindow
}

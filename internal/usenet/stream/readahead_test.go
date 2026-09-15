package stream

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

// peakCounter records the most fetches ever running at once.
type peakCounter struct{ cur, peak atomic.Int32 }

// peakFetcher is a pooled fetcher whose Body calls are slow enough to overlap
// and are counted in a counter shared between fetchers.
type peakFetcher struct {
	ArticleFetcher
	pool int
	c    *peakCounter
}

func (p peakFetcher) MaxConcurrency() int { return p.pool }

func (p peakFetcher) Body(ctx context.Context, id string) ([]byte, error) {
	n := p.c.cur.Add(1)
	defer p.c.cur.Add(-1)
	for old := p.c.peak.Load(); n > old && !p.c.peak.CompareAndSwap(old, n); old = p.c.peak.Load() {
	}
	time.Sleep(15 * time.Millisecond)
	return p.ArticleFetcher.Body(ctx, id)
}

// TestPrefetchGateFollowsPoolSize: a cache that outlives its connection pool
// resizes the gate for the new pool instead of keeping the first size.
func TestPrefetchGateFollowsPoolSize(t *testing.T) {
	c := NewArticleCache(16 << 20)
	if got := cap(c.prefetchGate(8)); got != 8 {
		t.Fatalf("gate for 8 slots has %d", got)
	}
	g := c.prefetchGate(8)
	if c.prefetchGate(8) != g {
		t.Fatal("same size: gate replaced")
	}
	if got := cap(c.prefetchGate(3)); got != 3 {
		t.Fatalf("after the pool shrank the gate has %d slots, want 3", got)
	}
	if got := cap(c.prefetchGate(0)); got != 1 {
		t.Fatalf("zero slots: gate has %d, want at least 1", got)
	}
}

// TestReadaheadGateBoundsFetchesAcrossReaders: read-ahead of every reader of a
// cache shares one set of slots, so connections stay free for articles a
// consumer is waiting on however many readers are reading ahead.
func TestReadaheadGateBoundsFetchesAcrossReaders(t *testing.T) {
	defer func(slots int) { ReadaheadMaxSlots = slots }(ReadaheadMaxSlots)
	ReadaheadMaxSlots = 2
	counter := &peakCounter{}
	cache := NewArticleCache(16 << 20)
	var readers []*Reader
	for i := 0; i < 2; i++ {
		rd, _, _ := newCountingReader(t, 50, 1024)
		rd.fetcher = peakFetcher{ArticleFetcher: rd.fetcher, pool: 10, c: counter}
		rd.cache.unretain()
		rd.cache = cache.NewScope()
		rd.cache.retain()
		readers = append(readers, rd)
	}
	readers[0].triggerReadahead(0, 1024)
	readers[1].triggerReadahead(20, 1024)
	for _, rd := range readers {
		rd.wg.Wait()
	}
	if got := counter.peak.Load(); got != 2 {
		t.Fatalf("at most %d read-ahead fetches ran at once, want exactly the gate's 2", got)
	}
}

// poolHint gives a fetcher a pool size, like *nntp.Client.
type poolHint struct {
	ArticleFetcher
	n int
}

func (p poolHint) MaxConcurrency() int { return p.n }

// TestReadaheadRampsWhileConsumerWaits: the window doubles, up to twice the pool
// (and ReadaheadMaxArticles), only while the consumer reads straight through and
// keeps waiting for articles; a seek brings it back to the base.
func TestReadaheadRampsWhileConsumerWaits(t *testing.T) {
	r, _, _ := newCountingReader(t, 50, 1024)
	r.readaheadBytes = 64 << 20

	step := func(seg int, waited bool, want int) {
		t.Helper()
		r.waited = waited
		r.rampReadahead(seg)
		if got := r.readaheadWindow(1024); got != want {
			t.Fatalf("after article %d (waited=%v): window %d, want %d", seg, waited, got, want)
		}
	}
	r.fetcher = poolHint{ArticleFetcher: r.fetcher, n: 6}
	step(0, true, 4) // a run shorter than the base window: no widening yet
	step(1, true, 4)
	step(2, true, 4)
	step(3, false, 4) // served from cache: no need to widen
	step(4, true, 8)  // outran the window after a long enough run: double
	step(4, true, 8)  // more Reads inside the same article change nothing
	step(5, true, 12) // capped at twice the pool
	step(6, true, 12)
	step(30, true, 4) // seek: back to the base window
	step(31, true, 4) // and the run starts over
}

// TestReadaheadRampNeedsAPool: without a pool big enough to widen into, the
// window stays at the base.
func TestReadaheadRampNeedsAPool(t *testing.T) {
	r, _, _ := newCountingReader(t, 50, 1024)
	r.readaheadBytes = 64 << 20
	for _, fetcher := range []ArticleFetcher{poolHint{ArticleFetcher: r.fetcher, n: 2}, struct{ ArticleFetcher }{r.fetcher}} {
		r.fetcher, r.raScale, r.lastSeg = fetcher, 1, -1
		for seg := 0; seg < 5; seg++ {
			r.waited = true
			r.rampReadahead(seg)
		}
		if got := r.readaheadWindow(1024); got != 4 {
			t.Errorf("%T: window %d, want the base 4", fetcher, got)
		}
	}
}

// TestReadaheadBudgetSharedByReaders: concurrent readers split the cache's
// read-ahead half, so their windows fit in it together.
func TestReadaheadBudgetSharedByReaders(t *testing.T) {
	r, _, _ := newCountingReader(t, 50, 1024)
	r.fetcher = poolHint{ArticleFetcher: r.fetcher, n: 10}
	r.readaheadBytes, r.raScale = 64<<20, 4
	cache := NewArticleCache(16 << 20)
	r.cache = cache.NewScope()
	r.cache.retain() // released by the reader's Close
	cache.countReadahead(1)
	if got := r.readaheadWindow(1 << 20); got != 8 {
		t.Fatalf("one reader, 16 MiB cache, 1 MiB articles: window %d, want 8", got)
	}
	cache.countReadahead(1)
	if got := r.readaheadWindow(1 << 20); got != 4 {
		t.Fatalf("two readers: window %d, want 4", got)
	}
	cache.countReadahead(-2)
}

// TestProbeReadersDoNotShrinkTheWindow: readers with read-ahead off (a RAR header
// probe) are not counted against a streaming reader's share; readers that read
// ahead are, until they close.
func TestProbeReadersDoNotShrinkTheWindow(t *testing.T) {
	cache := NewArticleCache(16 << 20)
	r, _, _ := newCountingReader(t, 50, 1024)
	probe, _, _ := newCountingReader(t, 50, 1024)
	for _, rd := range []*Reader{r, probe} {
		rd.cache.unretain()
		rd.cache = cache.NewScope()
		rd.cache.retain()
	}
	probe.DisableReadahead()
	probe.triggerReadahead(0, 1024)
	r.triggerReadahead(0, 1024)
	r.wg.Wait()
	if cache.readers != 1 {
		t.Fatalf("%d read-ahead readers counted, want 1 (the probe must not count)", cache.readers)
	}
	_ = r.Close()
	_ = probe.Close()
	if cache.readers != 0 {
		t.Fatalf("%d read-ahead readers counted after both closed, want 0", cache.readers)
	}
}

// TestIdleReaderReturnsItsBoost: a reader that stops reading hands its cache
// boost back without waiting for its next Read or Close.
func TestIdleReaderReturnsItsBoost(t *testing.T) {
	r, _, _ := newCountingReader(t, 50, 1024)
	r.fetcher = poolHint{ArticleFetcher: r.fetcher, n: 10}
	cache := NewArticleCache(16 << 20)
	cache.boostCeiling = 64 << 20
	r.cache.unretain()
	r.cache = cache.NewScope()
	r.cache.retain()
	r.readaheadBytes, r.raScale, r.idleWindow = 16<<20, 4, 20*time.Millisecond
	// The window is sized at grant time, so it proves the boost was borrowed even
	// if the idle timer has already handed it back.
	if got := r.readaheadWindow(4 << 20); got != 16 {
		t.Fatalf("widened window over 4 MiB articles: %d, want 16 (boost borrowed)", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		cache.mu.Lock()
		boosted := cache.boosted
		cache.mu.Unlock()
		if boosted == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("idle reader still holds its boost")
}

// TestWidenedWindowBorrowsCacheBoost: a widened window past the reader's byte
// budget borrows the rest from the cache's boost, and hands it back once the
// window is at the base again and when the reader closes.
func TestWidenedWindowBorrowsCacheBoost(t *testing.T) {
	r, _, _ := newCountingReader(t, 50, 1024)
	r.fetcher = poolHint{ArticleFetcher: r.fetcher, n: 10}
	cache := NewArticleCache(16 << 20)
	cache.boostCeiling = 64 << 20
	r.cache.unretain()
	r.cache = cache.NewScope()
	r.cache.retain()
	r.readaheadBytes, r.raScale = 16<<20, 4

	const article = 4 << 20
	if got := r.readaheadWindow(article); got != 16 {
		t.Fatalf("widened window over 4 MiB articles: %d, want 16", got)
	}
	if want := int64(16*article - 8<<20); r.raBoost != want || cache.boosted != want {
		t.Fatalf("boost held %d (cache %d), want %d", r.raBoost, cache.boosted, want)
	}
	r.raScale = 1
	if got := r.readaheadWindow(article); got != 2 {
		t.Fatalf("base window over 4 MiB articles in an 8 MiB share: %d, want 2", got)
	}
	if r.raBoost != 0 || cache.boosted != 0 {
		t.Fatalf("base window still holds boost %d (cache %d)", r.raBoost, cache.boosted)
	}
	r.raScale = 4
	r.readaheadWindow(article)
	_ = r.Close()
	if cache.boosted != 0 || cache.maxBytes != 16<<20 {
		t.Fatalf("after Close: boosted %d, bound %d", cache.boosted, cache.maxBytes)
	}
}

// TestReadaheadNarrowsWhenConsumerStopsWaiting: once a whole window's worth of
// articles is served without a wait, the widened window halves.
func TestReadaheadNarrowsWhenConsumerStopsWaiting(t *testing.T) {
	r, _, _ := newCountingReader(t, 100, 1024)
	r.fetcher = poolHint{ArticleFetcher: r.fetcher, n: 10}
	r.raScale, r.lastSeg, r.seqRun = 4, 9, 10
	for seg := 10; seg < 10+16; seg++ {
		if r.raScale != 4 {
			t.Fatalf("narrowed after %d calm articles, want 16", seg-10)
		}
		r.waited = false
		r.rampReadahead(seg)
	}
	if r.raScale != 2 {
		t.Fatalf("scale %d after 16 calm articles, want 2", r.raScale)
	}
	r.waited = true
	r.rampReadahead(26)
	if r.calmRun != 0 || r.raScale != 4 {
		t.Fatalf("a wait left calm run %d, scale %d; want 0, 4", r.calmRun, r.raScale)
	}
}

// TestSeekDropsQueuedReadahead: a seek discards what was queued for the old
// position, and the next trigger queues again even on the same article.
func TestSeekDropsQueuedReadahead(t *testing.T) {
	r, _, _ := newCountingReader(t, 50, 1024)
	r.pq.pending = map[int]pendingArticle{7: {id: "x"}, 8: {id: "y"}}
	r.pq.lastFront = 6
	if _, err := r.Seek(40*1024, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if len(r.pq.pending) != 0 || r.pq.lastFront != -1 {
		t.Fatalf("after seek: %d pending, lastFront %d; want 0, -1", len(r.pq.pending), r.pq.lastFront)
	}
}

// TestIdleDropIsRequeued: articles dropped while the reader was idle are queued
// again on the next Read, even inside the same article.
func TestIdleDropIsRequeued(t *testing.T) {
	r, s, _ := newCountingReader(t, 50, 1024)
	r.idleWindow = 10 * time.Millisecond
	setLastRead := func(at time.Time) {
		r.mu.Lock()
		r.lastRead = at
		r.mu.Unlock()
	}
	setLastRead(time.Now().Add(-time.Hour))
	before := s.BodyCalls()
	r.triggerReadahead(9, 1024)
	r.wg.Wait()
	if got := s.BodyCalls() - before; got != 0 {
		t.Fatalf("idle reader fetched %d read-ahead articles", got)
	}
	setLastRead(time.Now())
	r.triggerReadahead(9, 1024)
	r.wg.Wait()
	if got := s.BodyCalls() - before; got != r.readaheadK {
		t.Fatalf("resumed reader fetched %d read-ahead articles, want %d", got, r.readaheadK)
	}
}

// TestResumeAfterIdleDoesNotWiden: the wait for the first article after a pause
// is not the consumer outrunning the read-ahead.
func TestResumeAfterIdleDoesNotWiden(t *testing.T) {
	r, s, _ := newCountingReader(t, 50, 1024)
	r.fetcher = poolHint{ArticleFetcher: r.fetcher, n: 10}
	r.idleWindow = 10 * time.Millisecond
	r.seqRun = 10
	r.mu.Lock()
	r.lastRead = time.Now().Add(-time.Hour)
	r.mu.Unlock()
	s.DelayNext(1, 30*time.Millisecond)
	if _, err := r.Read(make([]byte, 16)); err != nil {
		t.Fatal(err)
	}
	if r.raScale != 1 {
		t.Fatalf("window scale %d after resuming from a pause, want 1", r.raScale)
	}
}

// TestReadaheadBudgetFitsTheCache: the window never holds more than half the
// article cache, or its own articles would be evicted before they are read.
func TestReadaheadBudgetFitsTheCache(t *testing.T) {
	r, _, _ := newCountingReader(t, 50, 1024)
	r.cache = NewArticleCache(8 << 20).NewScope()
	r.readaheadBytes = 64 << 20
	if got := r.readaheadWindow(1 << 20); got != 4 {
		t.Fatalf("1 MiB articles with an 8 MiB cache: window %d, want 4", got)
	}
}

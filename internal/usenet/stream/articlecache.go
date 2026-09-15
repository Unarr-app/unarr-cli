package stream

import (
	"container/list"
	"context"
	"errors"
	"log"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/yenc"
)

// ArticleCacheBytes is the process-wide ceiling on decoded articles kept resident
// for streamable sources, summed across every source. A package var (like
// ReadaheadBytes) rather than a config key: it is read once, when the first
// streamable plan is built, so a caller/test can retune it before that without a
// rebuild. It defaults to 16-32 MiB sized from usable memory, or to
// UNARR_USENET_CACHE_MB (see cachesize.go).
var ArticleCacheBytes = defaultArticleCacheBytes()

// ReadaheadBoostBytes is how far past ArticleCacheBytes the shared cache may grow
// while readers widened for a consumer that outruns them hold boosts (see
// readaheadBoostForMemory). Read with ArticleCacheBytes.
var ReadaheadBoostBytes = defaultReadaheadBoostBytes()

// readerCacheBytes bounds the private cache of a standalone Reader (one opened
// with NewReader rather than through a plan): the old 16-article cap, in bytes.
const readerCacheBytes = 16 << 20

var (
	sharedCacheOnce sync.Once
	sharedCache     *ArticleCache
)

// sharedArticleCache returns the process-wide cache every streamable plan draws
// its scope from, so the byte ceiling holds however many sources are live.
func sharedArticleCache() *ArticleCache {
	sharedCacheOnce.Do(func() {
		sharedCache = NewArticleCache(ArticleCacheBytes)
		sharedCache.onEmpty = returnMemoryToOS
		sharedCache.boostCeiling = ReadaheadBoostBytes
		log.Printf("[usenet-stream] decoded article cache: %d MiB, +%d MiB for fast consumers (%s overrides)",
			ArticleCacheBytes>>20, ReadaheadBoostBytes>>20, ArticleCacheSizeEnv)
	})
	return sharedCache
}

// ArticleCache is a byte-bounded LRU of decoded yEnc parts, safe for concurrent
// use by any number of readers, with in-flight de-duplication: while one reader
// fetches an article, every other reader asking for it waits for that fetch
// instead of issuing its own BODY. Entries are partitioned into scopes (one per
// source) so a finished source can drop exactly its own articles.
//
// Why it exists: every /usenet request opens a fresh Reader, and a per-reader
// cache died with it — a player re-reading a range it had just been served, or
// ffmpeg probing then decoding the same bytes, went back to NNTP for articles we
// had decoded seconds earlier. Usenet is billed by volume.
type ArticleCache struct {
	mu       sync.Mutex
	maxBytes int64 // baseBytes plus the boosts readers hold
	used     int64
	lru      *list.List // of *cacheEntry, most recently used at the front
	entries  map[cacheKey]*list.Element
	flights  map[cacheKey]*flight
	dead     map[cacheKey]deadMark // dead-article memo (deadmemo.go)
	readers  int                   // readers with read-ahead on, across every scope

	// baseBytes is the bound the cache was built with. boostCeiling is how far
	// readers may grow it past that (resizeBoost), boosted how far they have.
	baseBytes, boostCeiling, boosted int64
	// gate bounds read-ahead fetches across every reader (prefetchGate).
	gate chan struct{}
	// refs counts the holders of each part whose buffer is recycled (bufpool.go).
	refs map[*yenc.Part]int

	// onEmpty, when set, runs (without c.mu) after a released source's articles
	// were dropped and nothing else is cached.
	onEmpty func()
}

// CacheScope is one source's view of an ArticleCache. Readers retain it while
// open. Release marks the source finished; the scope's articles are dropped as
// soon as it is released AND no reader holds it.
//
// Dropping them at Release time would be wrong: a source is routinely released
// while readers are still streaming from it (the session closes the handle before
// /stream lets go of the provider; re-registering an id displaces a provider
// mid-request). Such a reader would lose caching and de-duplication and re-fetch
// its whole ~750 KB article on every 32 KB http.ServeContent Read — ~23 BODY per
// article. So a released scope keeps working normally for its open readers, and
// memory is freed when the last of them closes. A released scope with no readers
// still serves correct bytes but caches nothing.
type CacheScope struct {
	c        *ArticleCache
	released bool // guarded by c.mu
	refs     int  // open readers; guarded by c.mu
}

type cacheKey struct {
	scope *CacheScope
	id    string // message-id: unique per article, so it needs no file/segment qualifier
}

type cacheEntry struct {
	key  cacheKey
	part *yenc.Part
	size int64
}

// flight is one in-progress fetch. done is closed by finish; part/err are set
// before that and read only after it.
type flight struct {
	done  chan struct{}
	part  *yenc.Part
	err   error
	retry bool // the error belonged to the fetching reader, not to the article

	// waiters is how many load calls wait for the part, each taking a reference
	// to it when the flight finishes; finished says it has. Both guarded by c.mu.
	waiters  int
	finished bool
}

// errPrefetchDropped completes the flight of a read-ahead that was never issued
// because its reader went idle. Waiters must fetch for themselves.
var errPrefetchDropped = errors.New("usenet reader: read-ahead dropped (reader idle)")

// NewArticleCache returns an empty cache holding at most maxBytes of decoded
// article data. A part larger than the whole bound is served but never stored.
func NewArticleCache(maxBytes int64) *ArticleCache {
	return &ArticleCache{
		maxBytes:  maxBytes,
		baseBytes: maxBytes,
		lru:       list.New(),
		entries:   make(map[cacheKey]*list.Element),
		flights:   make(map[cacheKey]*flight),
		dead:      make(map[cacheKey]deadMark),
		refs:      make(map[*yenc.Part]int),
	}
}

// NewScope opens a new, empty scope on c.
func (c *ArticleCache) NewScope() *CacheScope { return &CacheScope{c: c} }

// Bytes reports the decoded bytes currently held across all scopes.
func (c *ArticleCache) Bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

// Len reports how many articles are currently held across all scopes.
func (c *ArticleCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}

// Bytes reports the decoded bytes this scope currently holds. O(entries); meant
// for diagnostics and tests, not a hot path.
func (s *CacheScope) Bytes() int64 {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	var n int64
	for el := s.c.lru.Front(); el != nil; el = el.Next() {
		if e := el.Value.(*cacheEntry); e.key.scope == s {
			n += e.size
		}
	}
	return n
}

// Release marks the scope's source finished: its articles are freed now if no
// reader holds the scope, otherwise when the last one closes. Idempotent.
// In-flight fetches still complete for their waiters.
func (s *CacheScope) Release() {
	s.c.mu.Lock()
	s.released = true
	emptied := s.refs == 0 && s.purgeLocked()
	s.c.mu.Unlock()
	s.c.noteEmptied(emptied)
}

// retain registers an open reader. Pair with exactly one unretain.
func (s *CacheScope) retain() {
	s.c.mu.Lock()
	s.refs++
	s.c.mu.Unlock()
}

// readaheadShare is the read-ahead budget one open reader may take: half the
// cache's base bound divided among the readers holding it, so concurrent windows
// fit together instead of evicting each other's unread articles. A reader's boost
// comes on top (resizeBoost).
func (c *ArticleCache) readaheadShare() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.baseBytes / int64(2*max(1, c.readers))
}

// prefetchGate returns the semaphore that bounds read-ahead fetches across every
// reader of the cache, sized with slots. One gate for all readers is what keeps
// connections free for a seek: a per-reader bound still lets several readers (or
// one that just closed, whose fetches cannot be recalled mid-article) fill the
// pool between them. The shared cache outlives any one connection pool (new
// credentials rebuild it with another size), so a gate of the wrong size is
// replaced; workers holding the old one finish on it, briefly adding up.
func (c *ArticleCache) prefetchGate(slots int) chan struct{} {
	slots = max(1, slots)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gate == nil || cap(c.gate) != slots {
		c.gate = make(chan struct{}, slots)
	}
	return c.gate
}

// countReadahead adds delta to the readers sharing the read-ahead half. Only
// readers that actually read ahead count: a RAR header probe reads with it off
// and must not shrink a live stream's window while it runs.
func (c *ArticleCache) countReadahead(delta int) {
	c.mu.Lock()
	c.readers += delta
	c.mu.Unlock()
}

// resizeBoost moves one reader's boost from held to want bytes, as far as the
// boost ceiling left by the other readers allows, and returns what the reader
// now holds. The byte bound grows by the boosts held, and shrinks back —
// evicting the coldest articles — as they are returned.
func (c *ArticleCache) resizeBoost(held, want int64) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	grant := max(0, min(want, c.boostCeiling-(c.boosted-held)))
	c.boosted += grant - held
	c.maxBytes = c.baseBytes + c.boosted
	for c.used > c.maxBytes && c.lru.Len() > 0 {
		c.removeLocked(c.lru.Back())
	}
	return grant
}

// unretain drops an open reader, freeing a released scope's articles once it was
// the last one.
func (s *CacheScope) unretain() {
	s.c.mu.Lock()
	s.refs--
	emptied := s.dormantLocked() && s.purgeLocked()
	s.c.mu.Unlock()
	s.c.noteEmptied(emptied)
}

// dormantLocked reports a released scope no reader holds: nothing may be cached
// or registered in it any more.
func (s *CacheScope) dormantLocked() bool { return s.released && s.refs <= 0 }

// purgeLocked drops every article and in-flight registration of this scope. It
// reports whether that emptied the whole cache.
func (s *CacheScope) purgeLocked() bool {
	c := s.c
	removed := 0
	for el := c.lru.Front(); el != nil; {
		next := el.Next()
		if e := el.Value.(*cacheEntry); e.key.scope == s {
			c.removeLocked(el)
			removed++
		}
		el = next
	}
	for k := range c.flights {
		if k.scope == s {
			delete(c.flights, k)
		}
	}
	c.purgeDeadLocked(s)
	return removed > 0 && c.lru.Len() == 0 && len(c.flights) == 0
}

// noteEmptied runs onEmpty once a purge left the cache with nothing in it.
func (c *ArticleCache) noteEmptied(emptied bool) {
	if emptied && c.onEmpty != nil {
		c.onEmpty()
	}
}

// returningMemory is set while returnMemoryToOS runs.
var returningMemory atomic.Bool

// returnMemoryToOS hands freed heap back to the OS in the background, one run at
// a time. The shared cache calls it when the last source's articles are dropped:
// the runtime otherwise keeps that heap mapped for minutes, so an idle daemon
// would go on showing the resident size of its last stream.
func returnMemoryToOS() {
	if !returningMemory.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer returningMemory.Store(false)
		articleBuffers.drain()
		debug.FreeOSMemory()
	}()
}

// load returns the decoded article id, from the cache, by joining a fetch already
// in flight, or by running fetch itself and publishing the result to everyone
// waiting. A waiter whose leader failed for a reason of its own (cancelled,
// budget spent, dropped as idle) fetches for itself instead of inheriting it.
// A returned part is held for the caller, who must releasePart it once done.
func (s *CacheScope) load(ctx context.Context, id string, fetch func() (*yenc.Part, error)) (*yenc.Part, error) {
	for {
		part, fl, leader := s.begin(id, true)
		if part != nil {
			return part, nil
		}
		if leader {
			part, err := fetch()
			s.finish(id, fl, part, err)
			return part, err
		}
		select {
		case <-fl.done:
		case <-ctx.Done():
			s.abandon(fl)
			return nil, ctx.Err()
		}
		if fl.err == nil || !fl.retry {
			return fl.part, fl.err
		}
	}
}

// claim registers the caller as the fetcher of id when it is neither cached nor
// already in flight — the read-ahead entry point, which must decide synchronously
// whether to spawn a goroutine at all. A true return MUST be paired with finish.
func (s *CacheScope) claim(id string) (*flight, bool) {
	part, fl, leader := s.begin(id, false)
	return fl, part == nil && leader
}

// begin resolves id to a cached part, an existing flight to wait on (possibly an
// already-settled one carrying a memoised dead-article verdict), or a new flight
// the caller now leads. With hold, a cached part is retained for the caller and a
// joined flight counts it as a waiter.
func (s *CacheScope) begin(id string, hold bool) (*yenc.Part, *flight, bool) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	fl := &flight{done: make(chan struct{})}
	if s.dormantLocked() {
		return nil, fl, true // untracked: no reader left to share with or store for
	}
	key := cacheKey{scope: s, id: id}
	if dead, ok := c.deadLocked(key, time.Now()); ok {
		return nil, settledFlight(dead), false
	}
	if el, ok := c.entries[key]; ok {
		c.lru.MoveToFront(el)
		part := el.Value.(*cacheEntry).part
		if hold {
			c.retainLocked(part, 1)
		}
		return part, nil, false
	}
	if existing, ok := c.flights[key]; ok {
		if hold {
			existing.waiters++
		}
		return nil, existing, false
	}
	c.flights[key] = fl
	return nil, fl, true
}

// finish publishes a led flight's outcome: stores a successful part (evicting
// least-recently-used articles to stay within the byte bound) and wakes waiters.
func (s *CacheScope) finish(id string, fl *flight, part *yenc.Part, err error) {
	c := s.c
	c.mu.Lock()
	key := cacheKey{scope: s, id: id}
	if c.flights[key] == fl {
		delete(c.flights, key)
	}
	if !s.dormantLocked() {
		if err == nil && part != nil {
			c.storeLocked(key, part)
		} else if err != nil {
			c.markDeadLocked(key, err, time.Now())
		}
	}
	if err == nil && part != nil {
		c.retainLocked(part, fl.waiters)
	}
	fl.part, fl.err, fl.finished = part, err, true
	fl.retry = err != nil && leaderSpecific(err)
	c.mu.Unlock()
	close(fl.done)
}

// leaderSpecific reports whether a fetch error says something about the reader
// that ran it rather than about the article, so a waiter should not inherit it.
func leaderSpecific(err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrFetchBudgetExhausted) ||
		errors.Is(err, errPrefetchDropped)
}

// storeLocked inserts part as most recently used and evicts from the cold end
// until the cache is back within maxBytes. Size is the backing array's capacity:
// that is what the part actually keeps alive.
func (c *ArticleCache) storeLocked(key cacheKey, part *yenc.Part) {
	size := int64(cap(part.Data))
	if size > c.maxBytes {
		return
	}
	if el, ok := c.entries[key]; ok {
		c.removeLocked(el)
	}
	c.entries[key] = c.lru.PushFront(&cacheEntry{key: key, part: part, size: size})
	c.retainLocked(part, 1)
	c.used += size
	for c.used > c.maxBytes {
		c.removeLocked(c.lru.Back())
	}
}

func (c *ArticleCache) removeLocked(el *list.Element) {
	e := c.lru.Remove(el).(*cacheEntry)
	delete(c.entries, e.key)
	c.used -= e.size
	articleBuffers.put(c.releaseLocked(e.part))
}

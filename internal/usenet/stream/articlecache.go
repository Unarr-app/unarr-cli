package stream

import (
	"container/list"
	"context"
	"errors"
	"sync"

	"github.com/Unarr-app/unarr-cli/internal/usenet/yenc"
)

// ArticleCacheBytes is the process-wide ceiling on decoded articles kept resident
// for streamable sources, summed across every source. A package var (like
// ReadaheadBytes) rather than a config key: it is read once, when the first
// streamable plan is built, so a caller/test can retune it before that without a
// rebuild. 256 MiB is ~340 articles at the usual ~750 KB — minutes of 1080p video,
// enough that a player's seek-back or ffmpeg's second pass over a range costs
// nothing, and small enough for the NAS/SBC hosts the daemon runs on.
var ArticleCacheBytes int64 = 256 << 20

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
	sharedCacheOnce.Do(func() { sharedCache = NewArticleCache(ArticleCacheBytes) })
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
	maxBytes int64
	used     int64
	lru      *list.List // of *cacheEntry, most recently used at the front
	entries  map[cacheKey]*list.Element
	flights  map[cacheKey]*flight
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
}

// errPrefetchDropped completes the flight of a read-ahead that was never issued
// because its reader went idle. Waiters must fetch for themselves.
var errPrefetchDropped = errors.New("usenet reader: read-ahead dropped (reader idle)")

// NewArticleCache returns an empty cache holding at most maxBytes of decoded
// article data. A part larger than the whole bound is served but never stored.
func NewArticleCache(maxBytes int64) *ArticleCache {
	return &ArticleCache{
		maxBytes: maxBytes,
		lru:      list.New(),
		entries:  make(map[cacheKey]*list.Element),
		flights:  make(map[cacheKey]*flight),
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
	defer s.c.mu.Unlock()
	s.released = true
	if s.refs == 0 {
		s.purgeLocked()
	}
}

// retain registers an open reader. Pair with exactly one unretain.
func (s *CacheScope) retain() {
	s.c.mu.Lock()
	s.refs++
	s.c.mu.Unlock()
}

// unretain drops an open reader, freeing a released scope's articles once it was
// the last one.
func (s *CacheScope) unretain() {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	s.refs--
	if s.dormantLocked() {
		s.purgeLocked()
	}
}

// dormantLocked reports a released scope no reader holds: nothing may be cached
// or registered in it any more.
func (s *CacheScope) dormantLocked() bool { return s.released && s.refs <= 0 }

// purgeLocked drops every article and in-flight registration of this scope.
func (s *CacheScope) purgeLocked() {
	c := s.c
	for el := c.lru.Front(); el != nil; {
		next := el.Next()
		if e := el.Value.(*cacheEntry); e.key.scope == s {
			c.removeLocked(el)
		}
		el = next
	}
	for k := range c.flights {
		if k.scope == s {
			delete(c.flights, k)
		}
	}
}

// load returns the decoded article id, from the cache, by joining a fetch already
// in flight, or by running fetch itself and publishing the result to everyone
// waiting. A waiter whose leader failed for a reason of its own (cancelled,
// budget spent, dropped as idle) fetches for itself instead of inheriting it.
func (s *CacheScope) load(ctx context.Context, id string, fetch func() (*yenc.Part, error)) (*yenc.Part, error) {
	for {
		part, fl, leader := s.begin(id)
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
	part, fl, leader := s.begin(id)
	return fl, part == nil && leader
}

// begin resolves id to a cached part, an existing flight to wait on, or a new
// flight the caller now leads.
func (s *CacheScope) begin(id string) (*yenc.Part, *flight, bool) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	fl := &flight{done: make(chan struct{})}
	if s.dormantLocked() {
		return nil, fl, true // untracked: no reader left to share with or store for
	}
	key := cacheKey{scope: s, id: id}
	if el, ok := c.entries[key]; ok {
		c.lru.MoveToFront(el)
		return el.Value.(*cacheEntry).part, nil, false
	}
	if existing, ok := c.flights[key]; ok {
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
	if err == nil && part != nil && !s.dormantLocked() {
		c.storeLocked(key, part)
	}
	fl.part, fl.err = part, err
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
	c.used += size
	for c.used > c.maxBytes {
		c.removeLocked(c.lru.Back())
	}
}

func (c *ArticleCache) removeLocked(el *list.Element) {
	e := c.lru.Remove(el).(*cacheEntry)
	delete(c.entries, e.key)
	c.used -= e.size
}

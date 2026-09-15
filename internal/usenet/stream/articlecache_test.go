package stream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
	"github.com/Unarr-app/unarr-cli/internal/usenet/yenc"
)

// fixedPart returns a decoded part whose backing array is exactly n bytes.
func fixedPart(n int) *yenc.Part { return &yenc.Part{Begin: 1, End: int64(n), Data: make([]byte, n)} }

// TestArticleCacheStaysWithinByteBound: however many articles are loaded, the
// resident bytes never exceed the bound, the most recent articles survive and the
// oldest are evicted first.
func TestArticleCacheStaysWithinByteBound(t *testing.T) {
	const bound, size = 10 << 10, 1 << 10
	c := NewArticleCache(bound)
	s := c.NewScope()
	for i := 0; i < 50; i++ {
		id := string(rune('A' + i))
		if _, err := s.load(context.Background(), id, func() (*yenc.Part, error) { return fixedPart(size), nil }); err != nil {
			t.Fatalf("load %d: %v", i, err)
		}
		if got := c.Bytes(); got > bound {
			t.Fatalf("after %d loads cache holds %d bytes, bound is %d", i+1, got, bound)
		}
	}
	if c.Len() != bound/size {
		t.Fatalf("cache holds %d articles, want %d", c.Len(), bound/size)
	}
	if _, _, leader := s.begin(string(rune('A' + 49))); leader {
		t.Fatal("most recent article was evicted")
	}
	if _, fl, leader := s.begin("A"); !leader {
		t.Fatal("oldest article survived eviction")
	} else {
		s.finish("A", fl, nil, errors.New("test: abandon"))
	}

	// A part bigger than the whole bound is served, never stored.
	big, err := s.load(context.Background(), "huge", func() (*yenc.Part, error) { return fixedPart(bound + 1), nil })
	if err != nil || big == nil {
		t.Fatalf("oversized load: part=%v err=%v", big, err)
	}
	if c.Bytes() > bound {
		t.Fatalf("oversized part pushed cache to %d bytes", c.Bytes())
	}
}

// TestArticleCacheBoostGrowsAndShrinks: readers' boosts grow the byte bound up to
// the ceiling between them, and returning one shrinks the bound and evicts.
func TestArticleCacheBoostGrowsAndShrinks(t *testing.T) {
	const size = 1 << 10
	c := NewArticleCache(4 * size)
	c.boostCeiling = 8 * size
	s := c.NewScope()
	a := c.resizeBoost(0, 6*size)
	b := c.resizeBoost(0, 6*size)
	if a != 6*size || b != 2*size {
		t.Fatalf("boosts granted %d and %d, want %d and %d", a, b, 6*size, 2*size)
	}
	for i := 0; i < 20; i++ {
		id := string(rune('A' + i))
		if _, err := s.load(context.Background(), id, func() (*yenc.Part, error) { return fixedPart(size), nil }); err != nil {
			t.Fatal(err)
		}
	}
	if got := c.Bytes(); got != 12*size {
		t.Fatalf("boosted cache holds %d bytes, want %d", got, 12*size)
	}
	if got := c.resizeBoost(a, 0); got != 0 {
		t.Fatalf("returned boost left %d", got)
	}
	if got := c.Bytes(); got != 6*size {
		t.Fatalf("after returning a boost the cache holds %d bytes, want %d", got, 6*size)
	}
	if got := c.resizeBoost(b, 8*size); got != 8*size {
		t.Fatalf("freed ceiling granted %d, want %d", got, 8*size)
	}
}

// TestArticleCacheReleaseFreesScope: releasing a scope drops exactly its own
// articles, and a released scope serves loads without caching them again.
func TestArticleCacheReleaseFreesScope(t *testing.T) {
	c := NewArticleCache(1 << 20)
	a, b := c.NewScope(), c.NewScope()
	for _, s := range []*CacheScope{a, b} {
		for _, id := range []string{"x", "y", "z"} {
			_, _ = s.load(context.Background(), id, func() (*yenc.Part, error) { return fixedPart(1000), nil })
		}
	}
	if a.Bytes() != 3000 || b.Bytes() != 3000 {
		t.Fatalf("scope bytes a=%d b=%d, want 3000 each", a.Bytes(), b.Bytes())
	}
	a.Release()
	a.Release() // idempotent
	if a.Bytes() != 0 || c.Bytes() != 3000 || b.Bytes() != 3000 {
		t.Fatalf("after release a=%d b=%d total=%d, want 0/3000/3000", a.Bytes(), b.Bytes(), c.Bytes())
	}
	var fetched atomic.Int32
	for i := 0; i < 2; i++ {
		_, _ = a.load(context.Background(), "x", func() (*yenc.Part, error) { fetched.Add(1); return fixedPart(1000), nil })
	}
	if fetched.Load() != 2 || a.Bytes() != 0 {
		t.Fatalf("released scope: %d fetches, %d bytes; want 2 fetches (uncached), 0 bytes", fetched.Load(), a.Bytes())
	}
}

// TestArticleCacheConcurrentLoadsFetchOnce: many readers asking for the same
// article at once issue ONE fetch; everyone gets the part.
func TestArticleCacheConcurrentLoadsFetchOnce(t *testing.T) {
	s := NewArticleCache(1 << 20).NewScope()
	gate := make(chan struct{})
	var fetches atomic.Int32
	fetch := func() (*yenc.Part, error) {
		fetches.Add(1)
		<-gate
		return fixedPart(100), nil
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p, err := s.load(context.Background(), "same", fetch); err != nil || p == nil {
				t.Errorf("load: part=%v err=%v", p, err)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond) // let the waiters queue on the flight
	close(gate)
	wg.Wait()
	if got := fetches.Load(); got != 1 {
		t.Fatalf("8 concurrent loads issued %d fetches, want 1", got)
	}
}

// TestArticleCacheWaiterRetriesAfterLeaderCancel: a leader failing for a reason
// of its own (its request went away) must not fail the reader waiting on it.
func TestArticleCacheWaiterRetriesAfterLeaderCancel(t *testing.T) {
	s := NewArticleCache(1 << 20).NewScope()
	_, fl, leader := s.begin("id")
	if !leader {
		t.Fatal("first begin did not lead")
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.load(context.Background(), "id", func() (*yenc.Part, error) { return fixedPart(10), nil })
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	s.finish("id", fl, nil, context.Canceled)
	if err := <-done; err != nil {
		t.Fatalf("waiter inherited the leader's cancellation: %v", err)
	}

	// An article-level failure IS shared: waiters must not re-issue a doomed fetch.
	_, fl, _ = s.begin("gone")
	go func() {
		_, err := s.load(context.Background(), "gone", func() (*yenc.Part, error) { return fixedPart(10), nil })
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	notFound := errors.New("430 no such article")
	s.finish("gone", fl, nil, notFound)
	if err := <-done; !errors.Is(err, notFound) {
		t.Fatalf("waiter err = %v, want the leader's article error", err)
	}
}

// newPlanReaders builds a direct plan over the fake server and returns it with the
// server, so reader-level BODY counts can be asserted across requests.
func newPlanReaders(t *testing.T, parts, partSize int) (*StreamPlan, *nntptest.FakeServer, []byte) {
	t.Helper()
	content := patternBytes(parts*partSize + 321)
	n, articles := nntptest.BuildDirectFile("movie.mkv", content, partSize)
	s := nntptest.NewFakeServer(t)
	s.AddArticles(articles)
	plan := StreamPlanFromNZB(context.Background(), dialFake(t, s), n)
	if !plan.Streamable() {
		t.Fatalf("plan not streamable: %s", plan.Reason)
	}
	t.Cleanup(plan.Close)
	return plan, s, content
}

// readRange opens a plan reader the way http.ServeContent does (size, seek,
// sequential reads), waits for its read-ahead to finish naturally, and closes it.
func readRange(t *testing.T, plan *StreamPlan, lo, n int) []byte {
	t.Helper()
	rd := plan.Open(context.Background()).(*Reader)
	if _, err := rd.Seek(0, io.SeekEnd); err != nil {
		t.Fatalf("seek end: %v", err)
	}
	if _, err := rd.Seek(int64(lo), io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(rd, buf); err != nil {
		t.Fatalf("read [%d,+%d): %v", lo, n, err)
	}
	rd.wg.Wait()
	_ = rd.Close()
	return buf
}

// TestPlanReadersShareIndexAndCache: with read-ahead ON, a second reader over a
// range the first already served — read-ahead included — issues zero BODY.
func TestPlanReadersShareIndexAndCache(t *testing.T) {
	const partSize = 4096
	plan, s, content := newPlanReaders(t, 40, partSize)
	if got := s.BodyCalls(); got != 1 {
		t.Fatalf("plan cost %d BODY, want 1 (the size article)", got)
	}

	lo, n := 20*partSize+100, 3*partSize
	if got := readRange(t, plan, lo, n); !bytes.Equal(got, content[lo:lo+n]) {
		t.Fatal("cold range bytes differ")
	}
	cold := s.BodyCalls() - 1
	if got := readRange(t, plan, lo, n); !bytes.Equal(got, content[lo:lo+n]) {
		t.Fatal("warm range bytes differ")
	}
	warm := s.BodyCalls() - 1 - cold
	t.Logf("3-article range with read-ahead: cold %d BODY, warm %d BODY", cold, warm)
	if warm != 0 {
		t.Fatalf("warm re-read cost %d BODY, want 0", warm)
	}
	if plan.CachedBytes() <= 0 {
		t.Fatal("plan cache is empty after serving a range")
	}
	plan.Close()
	if plan.CachedBytes() != 0 {
		t.Fatalf("plan still holds %d cached bytes after Close", plan.CachedBytes())
	}
}

// TestReleasedSourceKeepsCachingForOpenReaders: a source released while a reader
// is still streaming (the session closed the handle, /stream still serves the
// provider) must not multiply that reader's traffic. With read-ahead on and
// http.ServeContent's 32 KB reads over 256 KB articles, losing the cache would
// re-fetch every article ~8 times plus a read-ahead fan-out per Read. Memory is
// freed once the last reader closes.
func TestReleasedSourceKeepsCachingForOpenReaders(t *testing.T) {
	const partSize, articles = 256 << 10, 8
	plan, s, content := newPlanReaders(t, 16, partSize)
	rd := plan.Open(context.Background()).(*Reader)
	rd.retryBackoff = time.Millisecond

	plan.Close() // released while rd is open
	before := s.BodyCalls()

	n := int64(articles * partSize)
	got := make([]byte, 0, n)
	buf := make([]byte, 32<<10)
	for int64(len(got)) < n {
		k, err := rd.Read(buf)
		got = append(got, buf[:k]...)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	rd.wg.Wait()
	if !bytes.Equal(got[:n], content[:n]) {
		t.Fatal("bytes differ after release")
	}
	fetched := s.BodyCalls() - before
	t.Logf("%d articles streamed after release: %d BODY (read-ahead included)", articles, fetched)
	// The head article is already cached by the plan; the rest cost one BODY each
	// plus at most the widest read-ahead window past the cursor. Losing the cache
	// would cost several BODY per article, far above this.
	if limit := articles + min(rd.rampCeiling(), ReadaheadMaxArticles); fetched > limit {
		t.Fatalf("released-but-open reader issued %d BODY for %d articles, want <= %d", fetched, articles, limit)
	}
	if plan.CachedBytes() <= 0 {
		t.Fatal("open reader lost its cache when the source was released")
	}

	_ = rd.Close()
	_ = rd.Close() // idempotent: must not drop a second reference
	if got := plan.CachedBytes(); got != 0 {
		t.Fatalf("%d bytes still cached after the last reader closed", got)
	}
}

// TestReadaheadRespectsCacheBound: a full sequential stream with read-ahead on,
// over a cache far smaller than the file, stays within the bound and still
// reproduces every byte.
func TestReadaheadRespectsCacheBound(t *testing.T) {
	const partSize = 2048
	content := patternBytes(60*partSize + 77)
	n, articles := nntptest.BuildDirectFile("movie.mkv", content, partSize)
	s := nntptest.NewFakeServer(t)
	s.AddArticles(articles)
	f := n.Files[0]

	bound := int64(5 * partSize)
	cache := NewArticleCache(bound)
	rd := openReader(context.Background(), readerSource{fetcher: dialFake(t, s), ix: NewOffsetIndex(f), cache: cache.NewScope()})
	rd.retryBackoff = time.Millisecond
	t.Cleanup(func() { _ = rd.Close() })

	got := make([]byte, 0, len(content))
	buf := make([]byte, 700)
	for {
		k, err := rd.Read(buf)
		got = append(got, buf[:k]...)
		if cache.Bytes() > bound {
			t.Fatalf("cache at %d bytes exceeds bound %d mid-stream", cache.Bytes(), bound)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	rd.wg.Wait()
	if !bytes.Equal(got, content) {
		t.Fatalf("streamed %d bytes, want %d; content mismatch", len(got), len(content))
	}
	if cache.Bytes() > bound {
		t.Fatalf("cache ended at %d bytes, bound %d", cache.Bytes(), bound)
	}
}

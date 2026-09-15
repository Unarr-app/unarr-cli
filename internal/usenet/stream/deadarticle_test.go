package stream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
	"github.com/Unarr-app/unarr-cli/internal/usenet/yenc"
)

// TestDeadMemoIsStickyBoundedAndReleased: an article-level verdict is fetched
// once and then served from the memo; transport errors are not memoised; a
// stall's verdict expires; the memo is bounded and released with its scope.
func TestDeadMemoIsStickyBoundedAndReleased(t *testing.T) {
	c := NewArticleCache(1 << 20)
	s := c.NewScope()

	fetches := 0
	dead := func() (*yenc.Part, error) {
		fetches++
		return nil, &ArticleUnavailableError{MessageID: "x", Err: errors.New("430")}
	}
	for range 3 {
		if _, err := s.load(context.Background(), "x", dead); !errors.Is(err, ErrArticleUnavailable) {
			t.Fatalf("load dead article: err = %v, want ErrArticleUnavailable", err)
		}
	}
	if fetches != 1 {
		t.Fatalf("dead article fetched %d times, want 1 (memoised)", fetches)
	}
	if _, ok := s.claim("x"); ok {
		t.Fatal("read-ahead could claim a memoised dead article")
	}

	transient := func() (*yenc.Part, error) { fetches++; return nil, errors.New("connection reset") }
	_, _ = s.load(context.Background(), "y", transient)
	_, _ = s.load(context.Background(), "y", transient)
	if fetches != 3 {
		t.Fatalf("transport error was memoised (%d fetches, want 3)", fetches)
	}

	now := time.Now()
	c.mu.Lock()
	c.markDeadLocked(cacheKey{scope: s, id: "z"}, &ArticleUnavailableError{MessageID: "z", Stalled: true}, now)
	_, live := c.deadLocked(cacheKey{scope: s, id: "z"}, now.Add(StalledArticleTTL/2))
	_, stillThere := c.deadLocked(cacheKey{scope: s, id: "z"}, now.Add(StalledArticleTTL+time.Second))
	for i := 0; i < maxDeadArticles+100; i++ {
		c.markDeadLocked(cacheKey{scope: s, id: strconv.Itoa(i)}, &ArticleUnavailableError{}, now)
	}
	size := len(c.dead)
	c.mu.Unlock()
	if !live || stillThere {
		t.Fatalf("stall verdict live=%v before TTL, present=%v after TTL; want true/false", live, stillThere)
	}
	if size > maxDeadArticles {
		t.Fatalf("memo holds %d verdicts, bound is %d", size, maxDeadArticles)
	}

	s.Release()
	c.mu.Lock()
	size = len(c.dead)
	c.mu.Unlock()
	if size != 0 {
		t.Fatalf("%d verdicts left after the scope was released", size)
	}
}

// TestTimeoutOnDeadPooledConnectionIsNotAStall: the first pooled connection was
// silently dropped while idle, so its BODY times out under the stall bound. The
// article is healthy: the read must succeed and nothing may be memoised.
func TestTimeoutOnDeadPooledConnectionIsNotAStall(t *testing.T) {
	const partSize = 4096
	content := patternBytes(4*partSize + 10)
	n, articles := nntptest.BuildDirectFile("movie.mkv", content, partSize)
	s := nntptest.NewFakeServer(t)
	s.AddArticles(articles)
	f := n.Files[0]
	cache := NewArticleCache(1 << 20)
	src := readerSource{fetcher: dialFake(t, s), ix: NewOffsetIndex(f), cache: cache.NewScope()}

	rd := openReader(context.Background(), src)
	rd.stallTimeout = 150 * time.Millisecond
	rd.retryBackoff = time.Millisecond
	rd.DisableReadahead()
	defer rd.Close()

	s.StallNext(1)
	buf := make([]byte, 64)
	if _, err := io.ReadFull(rd, buf); err != nil {
		t.Fatalf("healthy article after an idle-connection timeout: %v", err)
	}
	if !bytes.Equal(buf, content[:64]) {
		t.Fatal("wrong bytes after an idle-connection timeout")
	}
	cache.mu.Lock()
	memo := len(cache.dead)
	cache.mu.Unlock()
	if memo != 0 {
		t.Fatalf("%d verdicts memoised for a healthy article", memo)
	}
}

// TestStalledArticleIsBoundedAndMemoised: a server that never answers BODY for
// an article costs one stall bound, not 60 s x reconnect retry x 3 attempts, and
// the next reader gets the verdict with no network.
func TestStalledArticleIsBoundedAndMemoised(t *testing.T) {
	const partSize, stalled = 4096, 3
	content := patternBytes(8*partSize + 50)
	n, articles := nntptest.BuildDirectFile("movie.mkv", content, partSize)
	s := nntptest.NewFakeServer(t)
	s.AddArticles(articles)
	f := n.Files[0]
	s.StallArticle(f.Segments[stalled].MessageID)
	src := readerSource{fetcher: dialFake(t, s), ix: NewOffsetIndex(f), cache: NewArticleCache(1 << 20).NewScope()}

	read := func() (time.Duration, error) {
		rd := openReader(context.Background(), src)
		rd.stallTimeout = 150 * time.Millisecond
		rd.retryBackoff = time.Millisecond
		rd.DisableReadahead()
		defer rd.Close()
		if _, err := rd.Seek(int64(stalled*partSize+10), io.SeekStart); err != nil {
			t.Fatalf("seek: %v", err)
		}
		start := time.Now()
		_, err := rd.Read(make([]byte, 64))
		return time.Since(start), err
	}

	cold, err := read()
	var u *ArticleUnavailableError
	if !errors.As(err, &u) || !u.Stalled {
		t.Fatalf("read of a stalled article: err = %v, want a stalled ArticleUnavailableError", err)
	}
	if cold > 2*time.Second {
		t.Fatalf("stalled article took %s to fail, want about one 150ms stall bound", cold)
	}
	calls := s.BodyCalls()
	warm, err := read()
	t.Logf("stalled article: cold %s, warm %s (%d BODY after warm)", cold.Round(time.Millisecond), warm.Round(time.Millisecond), s.BodyCalls())
	if err == nil {
		t.Fatal("second read of a stalled article succeeded")
	}
	if s.BodyCalls() != calls {
		t.Fatalf("second read issued %d more BODY, want 0 (memoised)", s.BodyCalls()-calls)
	}
	if warm > 50*time.Millisecond {
		t.Fatalf("second read took %s, want < 50ms", warm)
	}
}

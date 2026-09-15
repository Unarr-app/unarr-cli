package engine

// Cross-request reuse invariants for the /usenet endpoint.
//
// Every /usenet request opens a fresh reader, and http.ServeContent starts each
// one with Seek(0, io.SeekEnd). Before the shared per-source state existed, that
// Seek re-fetched article 0 on EVERY request (the plan already knew the exact
// size), and a re-read of a range that had just been served went back to NNTP
// because the decoded-article cache lived and died with the reader. Usenet is
// billed by volume, so these pin the cost in BODY commands, per message-id.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
	"github.com/Unarr-app/unarr-cli/internal/usenet/stream"
)

// reusePartSize is the article size of the reuse fixture. 64 KiB ranges then span
// four articles, the same shape as the benchmark that found the refetch.
const reusePartSize = 16_384

// countingFetcher counts BODY calls per message-id in front of a real NNTP client
// talking to the fake server. delay, when set, holds every fetch open so two
// concurrent readers are guaranteed to overlap on the same article.
type countingFetcher struct {
	inner stream.ArticleFetcher
	delay time.Duration

	mu   sync.Mutex
	byID map[string]int
	sum  int
}

func (c *countingFetcher) Body(ctx context.Context, messageID string) ([]byte, error) {
	c.mu.Lock()
	c.byID[messageID]++
	c.sum++
	c.mu.Unlock()
	if c.delay > 0 {
		time.Sleep(c.delay)
	}
	return c.inner.Body(ctx, messageID)
}

func (c *countingFetcher) count(messageID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byID[messageID]
}

func (c *countingFetcher) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sum
}

// settle waits until no BODY has been issued for a short quiet period, so the
// read-ahead goroutines of a finished request are accounted before the next
// measurement starts.
func (c *countingFetcher) settle(t *testing.T) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	last := c.total()
	for time.Now().Before(deadline) {
		time.Sleep(150 * time.Millisecond)
		now := c.total()
		if now == last {
			return now
		}
		last = now
	}
	t.Fatalf("BODY calls never settled (still growing at %d)", last)
	return last
}

// reuseFixture is a registered direct-post /usenet source over a counting fetcher.
type reuseFixture struct {
	content []byte
	ids     []string // message-id of each article, in file order
	cf      *countingFetcher
	handle  *UsenetStreamHandle
	url     string
}

func newReuseFixture(t *testing.T, parts int, delay time.Duration) *reuseFixture {
	t.Helper()
	content := usenetTestData(parts*reusePartSize + 5_000)
	n, articles := nntptest.BuildDirectFile("movie.mkv", content, reusePartSize)
	cf := &countingFetcher{inner: dialFakeArticles(t, articles), delay: delay, byID: map[string]int{}}

	ss := NewStreamServer(0, 1)
	handle, err := BuildUsenetStream(context.Background(), cf, n, ss, "reuse")
	if err != nil {
		t.Fatalf("BuildUsenetStream: %v", err)
	}
	t.Cleanup(handle.Close)
	_, url := usenetFront(t, ss, "reuse")

	ids := make([]string, 0, len(n.Files[0].Segments))
	for _, s := range n.Files[0].Segments {
		ids = append(ids, s.MessageID)
	}
	return &reuseFixture{content: content, ids: ids, cf: cf, handle: handle, url: url}
}

// rangeGet issues one ranged GET for [lo, hi] and checks the body is byte-exact.
func (f *reuseFixture) rangeGet(t *testing.T, lo, hi int) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, f.url, nil)
	req.Header.Set("Range", "bytes="+strconv.Itoa(lo)+"-"+strconv.Itoa(hi))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("ranged GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(body, f.content[lo:hi+1]) {
		t.Fatalf("range [%d,%d] body mismatch: got %d bytes", lo, hi, len(body))
	}
}

// TestUsenetRangeRequestsDoNotRefetchSizeArticle: the plan pins the exact size by
// fetching article 0 once. No later request — HEAD or ranged GET, however many —
// may fetch it again unless it actually reads bytes from it.
func TestUsenetRangeRequestsDoNotRefetchSizeArticle(t *testing.T) {
	f := newReuseFixture(t, 48, 0)
	head := f.ids[0]
	if got := f.cf.count(head); got != 1 {
		t.Fatalf("plan fetched article 0 %d times, want 1", got)
	}
	base := f.cf.settle(t)

	req, _ := http.NewRequest(http.MethodHead, f.url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	resp.Body.Close()
	if got := f.cf.settle(t) - base; got != 0 {
		t.Errorf("HEAD issued %d BODY commands, want 0 (size is already exact)", got)
	}

	const span = 64 << 10
	for i, lo := range []int{200_000, 400_000, 600_000} {
		before := f.cf.settle(t)
		f.rangeGet(t, lo, lo+span-1)
		t.Logf("cold 64 KiB range #%d at %d: %d BODY", i+1, lo, f.cf.settle(t)-before)
	}
	if got := f.cf.count(head); got != 1 {
		t.Fatalf("article 0 fetched %d times across HEAD + 3 range requests, want 1 (the plan's)", got)
	}
}

// TestUsenetWarmRangeReReadIsFree: re-serving a range that was just served must
// come from the decoded-article cache, not NNTP. The range is the file's last
// article so no read-ahead is scheduled behind it and the count is exact.
func TestUsenetWarmRangeReReadIsFree(t *testing.T) {
	f := newReuseFixture(t, 12, 0)
	base := f.cf.settle(t)

	lo, hi := len(f.content)-5_000, len(f.content)-1
	f.rangeGet(t, lo, hi)
	cold := f.cf.settle(t) - base
	f.rangeGet(t, lo, hi)
	warm := f.cf.settle(t) - base - cold
	t.Logf("tail range: cold %d BODY, warm %d BODY", cold, warm)
	if cold != 1 {
		t.Errorf("cold tail range cost %d BODY, want 1 (only the tail article)", cold)
	}
	if warm != 0 {
		t.Errorf("warm re-read cost %d BODY, want 0", warm)
	}
}

// TestUsenetConcurrentReadersShareOneFetch: two requests reading the same
// article at the same time must issue ONE BODY between them.
func TestUsenetConcurrentReadersShareOneFetch(t *testing.T) {
	f := newReuseFixture(t, 24, 80*time.Millisecond)
	f.cf.settle(t)

	const seg = 17
	lo := seg * reusePartSize
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			f.rangeGet(t, lo, lo+1023)
		}()
	}
	close(start)
	wg.Wait()
	if got := f.cf.count(f.ids[seg]); got != 1 {
		t.Fatalf("article %d fetched %d times by two concurrent readers, want 1", seg, got)
	}
}

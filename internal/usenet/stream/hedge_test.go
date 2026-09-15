package stream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntp"
)

// slowFirstFetcher stalls the first fetch of every article; later fetches of it
// are prompt, like a provider that served one BODY slowly. It counts the fetches
// started for each article; with missing set, every fetch ends in a 430.
type slowFirstFetcher struct {
	ArticleFetcher
	stall   time.Duration
	missing bool
	mu      sync.Mutex
	calls   map[string]int
}

func newSlowFirst(inner ArticleFetcher, stall time.Duration) *slowFirstFetcher {
	return &slowFirstFetcher{ArticleFetcher: inner, stall: stall, calls: map[string]int{}}
}

func (f *slowFirstFetcher) wait(id string) error {
	f.mu.Lock()
	f.calls[id]++
	first := f.calls[id] == 1
	f.mu.Unlock()
	if first {
		time.Sleep(f.stall)
	}
	if f.missing {
		return &nntp.ArticleNotFoundError{MessageID: id, Code: 430}
	}
	return nil
}

func (f *slowFirstFetcher) started(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[id]
}

func (f *slowFirstFetcher) Body(ctx context.Context, id string) ([]byte, error) {
	if err := f.wait(id); err != nil {
		return nil, err
	}
	return f.ArticleFetcher.Body(ctx, id)
}

func (f *slowFirstFetcher) BodyInto(ctx context.Context, id string, buf []byte) ([]byte, error) {
	if err := f.wait(id); err != nil {
		return nil, err
	}
	return f.ArticleFetcher.(bufferedFetcher).BodyInto(ctx, id, buf)
}

// primeLatency records enough fast fetches for hedging to judge by.
func primeLatency(c *ArticleCache, d time.Duration) {
	for range minLatencySamples {
		c.latency.note(d)
	}
}

// hedgeReader is a pinned-map reader over large articles (pooled, tracked
// buffers) whose fetches go through a slowFirstFetcher, with fast history.
func hedgeReader(t *testing.T, stall time.Duration) (*Reader, *slowFirstFetcher, []byte, int) {
	t.Helper()
	const partSize = 96 << 10
	r, _, content := newCountingReader(t, 40, partSize)
	r.readaheadK = 0
	if _, err := r.Seek(0, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	f := newSlowFirst(r.fetcher, stall)
	r.fetcher = f
	primeLatency(r.cache.c, 10*time.Millisecond)
	return r, f, content, partSize
}

// TestSlowFetchIsHedged: a consumer waiting on a fetch far slower than recent
// ones is served by a second fetch instead of waiting the first one out; the part
// served stays intact after the slower fetch finishes, and what that fetch
// decoded is released.
func TestSlowFetchIsHedged(t *testing.T) {
	poisonReleasedBuffers.Store(true)
	t.Cleanup(func() { poisonReleasedBuffers.Store(false) })
	r, f, content, partSize := hedgeReader(t, 2*time.Second)
	pos := int64(30 * partSize)
	read := func() []byte {
		t.Helper()
		if _, err := r.Seek(pos, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, 10)
		if _, err := io.ReadFull(r, got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	start := time.Now()
	if !bytes.Equal(read(), content[pos:pos+10]) {
		t.Fatal("hedged read served the wrong bytes")
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("read took %v: the slow fetch was waited out", took)
	}
	if n := f.started(r.ix.Segment(30).MessageID); n != 2 {
		t.Fatalf("%d fetches of the landing article, want 2 (the slow one and its hedge)", n)
	}
	r.wg.Wait() // the slower fetch finishes and is released
	if !bytes.Equal(read(), content[pos:pos+10]) {
		t.Fatal("the served part changed once the slower fetch was released")
	}
	_ = r.Close()
	c := r.cache.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.refs) != c.lru.Len() {
		t.Fatalf("%d parts referenced, %d cached: a fetch nobody serves was not released", len(c.refs), c.lru.Len())
	}
}

// TestHedgeReturnsAMissingArticleAtOnce: when the second fetch learns the article
// is gone, the consumer hears it then, not once the stalled first fetch ends.
func TestHedgeReturnsAMissingArticleAtOnce(t *testing.T) {
	r, f, _, partSize := hedgeReader(t, 2*time.Second)
	f.missing = true
	if _, err := r.Seek(int64(30*partSize), io.SeekStart); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := r.Read(make([]byte, 1))
	if !errors.Is(err, ErrArticleUnavailable) {
		t.Fatalf("err = %v, want the article's unavailability", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("missing article reported after %v: the stalled fetch was waited out", took)
	}
}

// TestNoHedgeForAnArticleNobodyWaitsOn: a fetch left running in the background
// after its reader was served and moved on is not raced by a second one.
func TestNoHedgeForAnArticleNobodyWaitsOn(t *testing.T) {
	r, f, _, _ := hedgeReader(t, 600*time.Millisecond)
	seg := r.ix.Segment(30)
	part, err := r.fetchHedged(seg.MessageID, seg.Bytes, &flight{done: make(chan struct{})})
	if err != nil {
		t.Fatal(err)
	}
	r.cache.c.releasePart(part)
	r.wg.Wait()
	if n := f.started(seg.MessageID); n != 1 {
		t.Fatalf("%d fetches, want 1 (no hedge)", n)
	}
}

// TestNoHedgeWithoutHistorySmallPoolOrBudget: a reader hedges only with enough
// recent fetches to judge by, a pool that can run both fetches at once, and a
// player waiting (no fetch budget).
func TestNoHedgeWithoutHistorySmallPoolOrBudget(t *testing.T) {
	cases := map[string]func(r *Reader, f *slowFirstFetcher){
		"no history": func(r *Reader, _ *slowFirstFetcher) { r.cache.c.latency = latencyRing{} },
		"small pool": func(r *Reader, f *slowFirstFetcher) { r.fetcher = poolHint{ArticleFetcher: f, n: 2} },
		"budget":     func(r *Reader, _ *slowFirstFetcher) { r.SetFetchBudget(NewFetchBudget(1 << 30)) },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			r, f, _, partSize := hedgeReader(t, 600*time.Millisecond)
			setup(r, f)
			if _, err := r.Seek(int64(30*partSize), io.SeekStart); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Read(make([]byte, 1)); err != nil {
				t.Fatal(err)
			}
			r.wg.Wait()
			if n := f.started(r.ix.Segment(30).MessageID); n != 1 {
				t.Fatalf("%d fetches, want 1 (no hedge)", n)
			}
		})
	}
}

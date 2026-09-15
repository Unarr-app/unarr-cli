package stream

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
)

// TestRecycledBuffersNeverServeStaleBytes: concurrent readers over a cache small
// enough to evict constantly, with read-ahead on and every released buffer
// poisoned, must still reassemble the file byte for byte — a buffer recycled while
// anyone could still read it would show up as poison. Buffers must actually be
// reused, and once everything is closed no reference may be left behind.
func TestRecycledBuffersNeverServeStaleBytes(t *testing.T) {
	poisonReleasedBuffers.Store(true)
	t.Cleanup(func() { poisonReleasedBuffers.Store(false) })

	const partSize = 96 << 10 // above offHeapMinBytes: buffers are mapped
	content := patternBytes(24*partSize + 300)
	n, articles := nntptest.BuildDirectFile("movie.mkv", content, partSize)
	s := nntptest.NewFakeServer(t)
	s.AddArticles(articles)
	client := dialFake(t, s)
	ix := NewOffsetIndex(n.Files[0])
	cache := NewArticleCache(4 * partSize) // three articles at most
	scope := cache.NewScope()
	reusedBefore := articleBuffers.reused.Load()

	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := openReader(context.Background(), readerSource{fetcher: client, ix: ix, cache: scope})
			r.retryBackoff = time.Millisecond
			defer func() { _ = r.Close() }()
			lo := i * partSize / 2 // each reader starts elsewhere, mid-article
			if _, err := r.Seek(int64(lo), io.SeekStart); err != nil {
				t.Errorf("reader %d seek: %v", i, err)
				return
			}
			got, err := io.ReadAll(io.LimitReader(struct{ io.Reader }{r}, int64(len(content)-lo)))
			if err != nil {
				t.Errorf("reader %d: %v", i, err)
				return
			}
			if !bytes.Equal(got, content[lo:]) {
				t.Errorf("reader %d served bytes that differ from the file", i)
			}
		}()
	}
	wg.Wait()
	scope.Release()

	if articleBuffers.reused.Load() == reusedBefore {
		t.Fatal("no buffer was reused")
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.refs) != 0 {
		t.Fatalf("%d parts still referenced after every reader closed", len(cache.refs))
	}
}

// TestCachedArticleOutlivesItsEviction: a reader part-way through an article it
// took from the cache keeps serving it after another reader evicts it; the buffer
// is recycled only when the first reader moves on.
func TestCachedArticleOutlivesItsEviction(t *testing.T) {
	poisonReleasedBuffers.Store(true)
	t.Cleanup(func() { poisonReleasedBuffers.Store(false) })

	const partSize = 96 << 10 // above offHeapMinBytes: buffers are mapped
	content := patternBytes(8 * partSize)
	n, articles := nntptest.BuildDirectFile("movie.mkv", content, partSize)
	s := nntptest.NewFakeServer(t)
	s.AddArticles(articles)
	src := readerSource{fetcher: dialFake(t, s), ix: NewOffsetIndex(n.Files[0]), cache: NewArticleCache(4 * partSize).NewScope()}
	open := func() *Reader {
		r := openReader(context.Background(), src)
		r.readaheadK = 0
		t.Cleanup(func() { _ = r.Close() })
		return r
	}
	read := func(r *Reader, lo, n int) {
		t.Helper()
		got := make([]byte, n)
		if _, err := r.Seek(int64(lo), io.SeekStart); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(r, got); err != nil {
			t.Fatalf("read [%d,+%d): %v", lo, n, err)
		}
		if !bytes.Equal(got, content[lo:lo+n]) {
			t.Fatalf("read [%d,+%d) served bytes that differ from the file", lo, n)
		}
	}

	a, b := open(), open()
	read(b, 0, partSize)          // b fetches article 0 into the cache
	read(a, 0, partSize/2)        // a takes it from the cache
	read(b, partSize, 5*partSize) // b moves on and evicts it
	read(a, partSize/2, partSize/2)
}

// TestAbandonedWaiterReleasesItsReference: a waiter that gives up before the
// flight finishes is no longer counted; one that gives up after drops the
// reference the flight took for it. Either way the buffer is freed with the rest.
func TestAbandonedWaiterReleasesItsReference(t *testing.T) {
	c := NewArticleCache(1 << 20)
	s := c.NewScope()
	_, fl, _ := s.begin("a", true)
	_, _, _ = s.begin("a", true) // early waiter
	_, _, _ = s.begin("a", true) // late waiter
	s.abandon(fl)                // the early waiter leaves first

	part := fixedPart(64)
	c.trackPart(part)
	s.finish("a", fl, part, nil) // leader, cache and the late waiter
	s.abandon(fl)                // the late waiter leaves after
	c.releasePart(part)          // the leader is done
	if got := c.refs[part]; got != 1 {
		t.Fatalf("part has %d holders, want 1 (the cache)", got)
	}
	s.Release()
	if len(c.refs) != 0 || part.Data != nil {
		t.Fatalf("released scope left refs=%d data=%v", len(c.refs), part.Data != nil)
	}
}

// TestBufferPoolReusesOnlyFittingBuffers: a buffer is reused for a request it
// can hold without wasting more than an eighth of it.
func TestBufferPoolReusesOnlyFittingBuffers(t *testing.T) {
	var p bufferPool
	t.Cleanup(p.drain)
	src := p.get(800 << 10)
	held := cap(src)
	p.put(append(src, 1, 2, 3))
	if b := p.get(held + 1); cap(b) == held {
		t.Fatalf("too-small buffer served: cap %d", cap(b))
	} else {
		p.put(b)
	}
	if b := p.get(held / 2); cap(b) == held {
		t.Fatalf("oversized buffer served: cap %d", cap(b))
	} else {
		p.put(b)
	}
	b := p.get(held - held/10)
	if cap(b) != held || len(b) != 0 {
		t.Fatalf("fitting buffer not reused: cap %d (want %d) len %d", cap(b), held, len(b))
	}
	p.put(b)
}

// TestFullBufferPoolKeepsTheNewestBuffer: a pool filled with buffers nobody asks
// for still takes a fitting one, dropping the oldest instead.
func TestFullBufferPoolKeepsTheNewestBuffer(t *testing.T) {
	var p bufferPool
	for range maxPooledBuffers {
		p.put(make([]byte, 0, 100))
	}
	p.put(make([]byte, 0, 4096))
	if b := p.get(4000); cap(b) != 4096 {
		t.Fatalf("fitting buffer was not kept: got cap %d", cap(b))
	}
	if len(p.free) != maxPooledBuffers-1 {
		t.Fatalf("pool holds %d buffers, want %d", len(p.free), maxPooledBuffers-1)
	}
}

// TestBufferPoolIsBounded: idle buffers never hold more than the pool's bound;
// the ones it drops are freed (off the heap, unmapped).
func TestBufferPoolIsBounded(t *testing.T) {
	var p bufferPool
	bufs := make([][]byte, 100)
	for i := range bufs {
		bufs[i] = p.get(1 << 20)
	}
	for _, b := range bufs {
		p.put(b)
	}
	if p.bytes > maxPooledBufferBytes || len(p.free) > maxPooledBuffers {
		t.Fatalf("pool holds %d bytes in %d buffers", p.bytes, len(p.free))
	}
	p.drain()
}

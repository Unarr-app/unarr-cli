package stream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nzb"
)

// newDirectReader wires a Reader to a fake NNTP server serving a single video
// file split into uniform partSize articles. Read-ahead is off by default so
// BodyCalls assertions count only the articles the read path itself needs; tests
// that exercise read-ahead re-enable it. Retry backoff is tiny for speed.
func newDirectReader(t *testing.T, name string, content []byte, partSize int) (*Reader, *nntptest.FakeServer) {
	t.Helper()
	n, articles := nntptest.BuildDirectFile(name, content, partSize)
	s := nntptest.NewFakeServer(t)
	s.AddArticles(articles)
	c := dialFake(t, s)

	f := n.Files[0]
	r := NewReader(context.Background(), c, f, NewOffsetIndex(f))
	r.retryBackoff = time.Millisecond
	r.readaheadK = 0
	t.Cleanup(func() { _ = r.Close() })
	return r, s
}

func TestReaderSequentialReadExact(t *testing.T) {
	const partSize = 4096
	content := patternBytes(5*partSize + 913) // several full parts + a short tail
	r, _ := newDirectReader(t, "movie.mkv", content, partSize)

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("streamed %d bytes, want %d; content mismatch", len(got), len(content))
	}
}

func TestReaderSequentialReadExactTinyBuffers(t *testing.T) {
	// A pathological 100-byte reader across article boundaries must still
	// reassemble byte-for-byte (exercises the copy-remainder path).
	const partSize = 512
	content := patternBytes(3*partSize + 200)
	r, _ := newDirectReader(t, "clip.mp4", content, partSize)

	got := make([]byte, 0, len(content))
	buf := make([]byte, 100)
	for {
		n, err := r.Read(buf)
		got = append(got, buf[:n]...)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	}
	if !bytes.Equal(got, content) {
		t.Fatal("tiny-buffer stream did not reassemble to content")
	}
}

func TestReaderSeekEndReportsExactSize(t *testing.T) {
	const partSize = 4096
	content := patternBytes(4*partSize + 77)
	r, s := newDirectReader(t, "movie.mkv", content, partSize)

	// http.ServeContent's first move: Seek(0, SeekEnd) to learn the size. It must
	// be byte-exact (not the ~3%-high Segment.Bytes estimate) and cost 1 article.
	end, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		t.Fatalf("Seek end: %v", err)
	}
	if end != int64(len(content)) {
		t.Fatalf("Seek(0,End) = %d, want exact %d", end, len(content))
	}
	if !r.ix.SizeExact() {
		t.Fatal("size must be exact after Seek(0,End)")
	}
	if s.BodyCalls() != 1 {
		t.Fatalf("Seek(0,End) fetched %d articles, want 1", s.BodyCalls())
	}
}

func TestReaderSeekForwardAndBack(t *testing.T) {
	const partSize = 4096
	content := patternBytes(6*partSize + 321)
	r, _ := newDirectReader(t, "movie.mkv", content, partSize)

	read := func(from, n int64) []byte {
		if _, err := r.Seek(from, io.SeekStart); err != nil {
			t.Fatalf("Seek(%d): %v", from, err)
		}
		buf := make([]byte, n)
		got, err := io.ReadFull(r, buf)
		if err != nil {
			t.Fatalf("ReadFull @%d: %v", from, err)
		}
		return buf[:got]
	}

	// Jump deep into the file, then seek backwards — both must return exact slices.
	mid := int64(4*partSize + 100)
	if g := read(mid, 500); !bytes.Equal(g, content[mid:mid+500]) {
		t.Fatal("forward seek returned wrong bytes")
	}
	back := int64(partSize + 50)
	if g := read(back, 800); !bytes.Equal(g, content[back:back+800]) {
		t.Fatal("backward seek returned wrong bytes")
	}
	// A seek that spans an article boundary.
	span := int64(2*partSize - 20)
	if g := read(span, 60); !bytes.Equal(g, content[span:span+60]) {
		t.Fatal("boundary-spanning read returned wrong bytes")
	}
}

func TestReaderSeekMiddleFetchesOneArticle(t *testing.T) {
	const partSize = 4096
	content := patternBytes(8 * partSize) // uniform: one observe pins the whole map
	r, s := newDirectReader(t, "movie.mkv", content, partSize)

	// Warm the size (as ServeContent does). For a uniform posting this single
	// observe makes every article boundary exact.
	if _, err := r.Seek(0, io.SeekEnd); err != nil {
		t.Fatalf("warm seek: %v", err)
	}
	base := s.BodyCalls()

	// A seek+read wholly inside article 5 must fetch exactly that one article.
	off := int64(5*partSize + 128)
	if _, err := r.Seek(off, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	buf := make([]byte, 200)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if !bytes.Equal(buf, content[off:off+200]) {
		t.Fatal("mid read returned wrong bytes")
	}
	if delta := s.BodyCalls() - base; delta != 1 {
		t.Fatalf("mid seek fetched %d articles, want 1", delta)
	}
}

func TestReaderTransientArticleMissingRetries(t *testing.T) {
	const partSize = 4096
	content := patternBytes(2 * partSize)
	r, s := newDirectReader(t, "movie.mkv", content, partSize)

	// The first article is briefly missing (430) twice — propagation delay — then
	// appears. The read must transparently retry and still return exact bytes.
	s.FailNext(2, 430)

	buf := make([]byte, 1000)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("ReadFull after transient miss: %v", err)
	}
	if !bytes.Equal(buf, content[:1000]) {
		t.Fatal("bytes after retry do not match content")
	}
	if s.BodyCalls() != 3 {
		t.Fatalf("BodyCalls = %d, want 3 (2 failures + 1 success)", s.BodyCalls())
	}
}

func TestReaderDroppedConnectionRecovers(t *testing.T) {
	const partSize = 4096
	content := patternBytes(2 * partSize)
	r, s := newDirectReader(t, "movie.mkv", content, partSize)

	// A mid-request connection drop (code <= 0) is healed by the nntp client's own
	// reconnect+retry, transparent to the reader — the read still succeeds and
	// returns exact bytes.
	s.FailNext(1, 0)

	buf := make([]byte, 1000)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("ReadFull after connection drop: %v", err)
	}
	if !bytes.Equal(buf, content[:1000]) {
		t.Fatal("bytes after connection-drop recovery do not match content")
	}
}

func TestReaderPermanentMissingArticleErrors(t *testing.T) {
	const partSize = 4096
	content := patternBytes(2 * partSize)
	r, s := newDirectReader(t, "movie.mkv", content, partSize)

	// Article stays missing beyond every retry — the read must surface an error,
	// never hang, so the caller can fall back to the batch download.
	s.FailNext(100, 430)

	buf := make([]byte, 100)
	_, err := r.Read(buf)
	if err == nil {
		t.Fatal("expected an error for a permanently missing article")
	}
	if errors.Is(err, io.EOF) {
		t.Fatalf("missing article surfaced as EOF, want a real error: %v", err)
	}
}

func TestReaderReadaheadPopulatesCache(t *testing.T) {
	const partSize = 4096
	content := patternBytes(8 * partSize)
	r, s := newDirectReader(t, "movie.mkv", content, partSize)
	r.readaheadK = 4

	// Warm size so the map is exact, then read inside article 0. Read-ahead should
	// prefetch articles 1..4 in the background.
	if _, err := r.Seek(0, io.SeekEnd); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("rewind: %v", err)
	}
	buf := make([]byte, 100)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("read seg0: %v", err)
	}
	r.wg.Wait() // let read-ahead settle

	afterPrefetch := s.BodyCalls()
	if afterPrefetch <= 1 {
		t.Fatalf("read-ahead did not prefetch (BodyCalls=%d)", afterPrefetch)
	}

	// Disable read-ahead before the cache-hit measurement. Reading 1..4 would
	// otherwise legitimately prefetch 5,6,7 in the background, and those BODY
	// calls would race this assertion (that is the test's flakiness, not a
	// product bug). The wg.Wait above guarantees no read-ahead goroutine is live,
	// so this write is race-free, and with K=0 the reads below spawn none.
	r.readaheadK = 0

	// Reading straight through articles 1..4 must now be served from cache: no new
	// BODY calls.
	if _, err := r.Seek(int64(partSize), io.SeekStart); err != nil {
		t.Fatalf("seek seg1: %v", err)
	}
	span := make([]byte, 4*partSize)
	if _, err := io.ReadFull(r, span); err != nil {
		t.Fatalf("read seg1..4: %v", err)
	}
	if !bytes.Equal(span, content[partSize:5*partSize]) {
		t.Fatal("cached read returned wrong bytes")
	}
	if s.BodyCalls() != afterPrefetch {
		t.Fatalf("cached read issued %d new BODY calls, want 0", s.BodyCalls()-afterPrefetch)
	}
}

func TestReaderReadaheadFullStreamStillExact(t *testing.T) {
	const partSize = 2048
	content := patternBytes(7*partSize + 555)
	r, _ := newDirectReader(t, "movie.mkv", content, partSize)
	r.readaheadK = 3 // read-ahead racing the read path must not corrupt output

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("read-ahead stream did not reassemble to content")
	}
}

func TestReaderCloseStopsReadahead(t *testing.T) {
	const partSize = 4096
	content := patternBytes(20 * partSize)
	r, _ := newDirectReader(t, "movie.mkv", content, partSize)
	r.readaheadK = 8

	buf := make([]byte, 100)
	if _, err := r.Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	// Close must cancel and join the read-ahead goroutines without deadlocking.
	done := make(chan error, 1)
	go func() { done <- r.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung waiting on read-ahead")
	}
}

func TestReaderEmptyFileImmediateEOF(t *testing.T) {
	r := NewReader(context.Background(), nil, nzb.File{}, NewOffsetIndex(nzb.File{}))
	t.Cleanup(func() { _ = r.Close() })
	if _, err := r.Read(make([]byte, 10)); !errors.Is(err, io.EOF) {
		t.Fatalf("empty file Read err = %v, want io.EOF", err)
	}
}

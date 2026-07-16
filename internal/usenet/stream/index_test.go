package stream

import (
	"context"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntp"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nzb"
	"github.com/Unarr-app/unarr-cli/internal/usenet/yenc"
)

// patternBytes builds deterministic, varied content that exercises the full byte
// range (so yEnc escapes and CRCs are non-trivial) at length n.
func patternBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((i*7 + 3) & 0xff)
	}
	return b
}

// dialFake connects a real nntp.Client to a FakeServer, exercising the actual
// fetch path in these index tests.
func dialFake(t *testing.T, s *nntptest.FakeServer) *nntp.Client {
	t.Helper()
	c := nntp.NewClient(s.Config())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// fetchPart fetches and decodes the article backing segment segIdx via the fake
// server, returning the yEnc part the OffsetIndex observes.
func fetchPart(t *testing.T, c *nntp.Client, ix *OffsetIndex, segIdx int) *yenc.Part {
	t.Helper()
	raw, err := c.Body(context.Background(), ix.Segment(segIdx).MessageID)
	if err != nil {
		t.Fatalf("fetch segment %d: %v", segIdx, err)
	}
	part, err := yenc.DecodeBytes(raw)
	if err != nil {
		t.Fatalf("decode segment %d: %v", segIdx, err)
	}
	return part
}

func TestOffsetIndexEmpty(t *testing.T) {
	ix := NewOffsetIndex(nzb.File{})
	if ix.SegmentCount() != 0 {
		t.Fatalf("SegmentCount = %d, want 0", ix.SegmentCount())
	}
	if ix.FileSize() != 0 {
		t.Fatalf("FileSize = %d, want 0", ix.FileSize())
	}
	if _, _, _, ok := ix.Locate(0); ok {
		t.Fatal("Locate(0) on empty index must be not-found")
	}
}

func TestOffsetIndexSortsByPartNumber(t *testing.T) {
	// Segments arrive out of order; the index must assemble them by Number so
	// offset 0 maps to part 1.
	f := nzb.File{Segments: []nzb.Segment{
		{Number: 3, Bytes: 10, MessageID: "c"},
		{Number: 1, Bytes: 10, MessageID: "a"},
		{Number: 2, Bytes: 10, MessageID: "b"},
	}}
	ix := NewOffsetIndex(f)
	if got := ix.Segment(0).MessageID; got != "a" {
		t.Fatalf("segment[0] = %q, want a", got)
	}
	if got := ix.Segment(2).MessageID; got != "c" {
		t.Fatalf("segment[2] = %q, want c", got)
	}
}

func TestOffsetIndexEstimateBeforeObserve(t *testing.T) {
	content := patternBytes(20000)
	n, _ := nntptest.BuildDirectFile("clip.mkv", content, 4096)
	ix := NewOffsetIndex(n.Files[0])

	if ix.SizeExact() {
		t.Fatal("SizeExact must be false before any Observe")
	}
	// Encoded segment bytes exceed the decoded payload, so the estimate is an
	// over-count of the real content length — never an under-count (which would
	// let Locate fall off the end).
	if ix.FileSize() < int64(len(content)) {
		t.Fatalf("estimated FileSize %d < content %d", ix.FileSize(), len(content))
	}
	// Offset 0 always resolves to the first segment; a monotonic map covers the
	// whole estimated range.
	if idx, start, _, ok := ix.Locate(0); !ok || idx != 0 || start != 0 {
		t.Fatalf("Locate(0) = (%d,%d,%v), want (0,0,true)", idx, start, ok)
	}
	if _, _, _, ok := ix.Locate(ix.FileSize()); ok {
		t.Fatal("Locate at FileSize (EOF) must be not-found")
	}
	if _, _, _, ok := ix.Locate(-1); ok {
		t.Fatal("Locate(-1) must be not-found")
	}
}

func TestOffsetIndexUniformOneObserveFixesWholeMap(t *testing.T) {
	const partSize = 4096
	content := patternBytes(5*partSize + 777) // 5 full parts + a short tail
	n, articles := nntptest.BuildDirectFile("movie.mkv", content, partSize)
	s := nntptest.NewFakeServer(t)
	s.AddArticles(articles)
	c := dialFake(t, s)

	ix := NewOffsetIndex(n.Files[0])
	// Observe a single NON-final segment; a uniform posting must pin the whole map.
	ix.Observe(1, fetchPart(t, c, ix, 1))

	if s.BodyCalls() != 1 {
		t.Fatalf("BodyCalls = %d, want 1 (one article pins a uniform map)", s.BodyCalls())
	}
	if !ix.SizeExact() || ix.FileSize() != int64(len(content)) {
		t.Fatalf("FileSize = %d (exact=%v), want %d exact", ix.FileSize(), ix.SizeExact(), len(content))
	}
	assertExactContiguousMap(t, ix, partSize, len(content))
}

func TestOffsetIndexNonUniformProgressiveExact(t *testing.T) {
	// A posting whose parts differ in size: no single observation can pin it, so
	// each segment becomes exact only once it (or a pin either side) is observed.
	partSizes := []int{5000, 1200, 8000, 400, 3333}
	f, articles, content := buildNonUniform(t, "irregular.mkv", partSizes)
	s := nntptest.NewFakeServer(t)
	s.AddArticles(articles)
	c := dialFake(t, s)

	ix := NewOffsetIndex(f)
	// Observe out of order to prove pinning is per-segment, not sequential.
	order := []int{2, 0, 4, 1, 3}
	for _, segIdx := range order {
		ix.Observe(segIdx, fetchPart(t, c, ix, segIdx))
	}

	if !ix.SizeExact() || ix.FileSize() != int64(len(content)) {
		t.Fatalf("FileSize = %d (exact=%v), want %d exact", ix.FileSize(), ix.SizeExact(), len(content))
	}
	// Fully observed: the map is byte-exact for every part boundary.
	want := int64(0)
	for i, ps := range partSizes {
		idx, start, end, ok := ix.Locate(want)
		if !ok || idx != i || start != want || end != want+int64(ps) {
			t.Fatalf("segment %d: Locate(%d) = (%d,%d,%d,%v), want (%d,%d,%d,true)",
				i, want, idx, start, end, ok, i, want, want+int64(ps))
		}
		want += int64(ps)
	}
	if want != int64(len(content)) {
		t.Fatalf("part sizes sum to %d, content is %d", want, len(content))
	}
}

func TestOffsetIndexPartialObserveIsExactForPinnedSegment(t *testing.T) {
	// Observing one segment of a non-uniform file must make THAT segment's range
	// exact immediately, even while its neighbours are still estimated.
	partSizes := []int{5000, 1200, 8000}
	f, articles, _ := buildNonUniform(t, "part.mkv", partSizes)
	s := nntptest.NewFakeServer(t)
	s.AddArticles(articles)
	c := dialFake(t, s)

	ix := NewOffsetIndex(f)
	ix.Observe(1, fetchPart(t, c, ix, 1)) // pin the middle part

	// Segment 1 spans exactly [5000, 6200); Locate inside it must be exact.
	idx, start, end, ok := ix.Locate(5500)
	if !ok || idx != 1 || start != 5000 || end != 6200 {
		t.Fatalf("Locate(5500) = (%d,%d,%d,%v), want (1,5000,6200,true)", idx, start, end, ok)
	}
}

func TestOffsetIndexObserveIgnoresInvalidInput(t *testing.T) {
	content := patternBytes(9000)
	n, _ := nntptest.BuildDirectFile("v.mkv", content, 4096)
	ix := NewOffsetIndex(n.Files[0])
	before := ix.FileSize()

	// None of these must panic or mutate the map.
	ix.Observe(0, nil)
	ix.Observe(-1, &yenc.Part{Begin: 1, End: 10, Size: 9000})
	ix.Observe(999, &yenc.Part{Begin: 1, End: 10, Size: 9000})
	ix.Observe(0, &yenc.Part{Begin: 0, End: 10}) // begin must be >= 1
	ix.Observe(0, &yenc.Part{Begin: 10, End: 5}) // end < begin

	if ix.SizeExact() {
		t.Fatal("no valid Observe happened; SizeExact must stay false")
	}
	if ix.FileSize() != before {
		t.Fatalf("FileSize changed after invalid observes: %d -> %d", before, ix.FileSize())
	}
}

// --- helpers ---

// assertExactContiguousMap verifies a uniform file's map: parts 0..n-2 are
// partSize long, the tail fills the remainder, and ranges tile [0,fileSize)
// with no gaps or overlaps.
func assertExactContiguousMap(t *testing.T, ix *OffsetIndex, partSize, total int) {
	t.Helper()
	n := ix.SegmentCount()
	var cursor int64
	for i := 0; i < n; i++ {
		idx, start, end, ok := ix.Locate(cursor)
		if !ok || idx != i || start != cursor {
			t.Fatalf("part %d: Locate(%d) = (%d,%d,_,%v), want start %d", i, cursor, idx, start, ok, cursor)
		}
		wantLen := int64(partSize)
		if i == n-1 {
			wantLen = int64(total) - cursor
		}
		if end-start != wantLen {
			t.Fatalf("part %d: length %d, want %d", i, end-start, wantLen)
		}
		cursor = end
	}
	if cursor != int64(total) {
		t.Fatalf("map covers %d bytes, want %d", cursor, total)
	}
}

// buildNonUniform posts a single logical file split into parts of the exact
// given sizes (unlike BuildDirectFile, which uses one uniform partSize). It
// returns the nzb.File, the message-id -> yEnc body map for the fake server, and
// the full content. Segment.Bytes is the ENCODED size, so the pre-observation
// estimate is deliberately off and must be corrected by Observe.
func buildNonUniform(t *testing.T, name string, partSizes []int) (nzb.File, map[string][]byte, []byte) {
	t.Helper()
	total := 0
	for _, ps := range partSizes {
		total += ps
	}
	content := patternBytes(total)
	articles := make(map[string][]byte, len(partSizes))
	segs := make([]nzb.Segment, 0, len(partSizes))

	offset := 0
	for i, ps := range partSizes {
		partNum := i + 1
		begin := int64(offset) + 1
		end := int64(offset + ps)
		body := yenc.Encode(name, partNum, len(partSizes), begin, end, int64(total), content[offset:offset+ps])
		id := "nonuni-" + name + "-p" + itoa(partNum)
		articles[id] = body
		segs = append(segs, nzb.Segment{Bytes: int64(len(body)), Number: partNum, MessageID: id})
		offset += ps
	}
	f := nzb.File{
		Subject:  `[nntptest] "` + name + `" yEnc (1/` + itoa(len(partSizes)) + `)`,
		Segments: segs,
	}
	return f, articles, content
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

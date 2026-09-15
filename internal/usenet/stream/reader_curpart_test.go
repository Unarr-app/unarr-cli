package stream

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
	"github.com/Unarr-app/unarr-cli/internal/usenet/yenc"
)

// TestReaderKeepsCurrentArticleWithoutCache: the many small Reads over one article
// must not re-fetch it when the shared cache cannot hold it (evicted, or larger
// than the whole bound) — each article is fetched exactly once.
func TestReaderKeepsCurrentArticleWithoutCache(t *testing.T) {
	const partSize = 2048
	content := patternBytes(4*partSize + 300)
	n, articles := nntptest.BuildDirectFile("movie.mkv", content, partSize)
	s := nntptest.NewFakeServer(t)
	s.AddArticles(articles)
	f := n.Files[0]

	scope := NewArticleCache(1).NewScope() // stores nothing
	r := openReader(context.Background(), readerSource{fetcher: dialFake(t, s), ix: NewOffsetIndex(f), cache: scope})
	r.retryBackoff = time.Millisecond
	r.readaheadK = 0
	t.Cleanup(func() { _ = r.Close() })

	got, err := io.ReadAll(io.LimitReader(struct{ io.Reader }{r}, int64(len(content))+1))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("stream did not reassemble to content")
	}
	if calls, want := s.BodyCalls(), len(f.Segments); calls != want {
		t.Fatalf("BodyCalls = %d, want %d (one per article)", calls, want)
	}
}

// TestDecodeArticleTrimsOversizedBuffer: a body buffer sized from an NZB that
// overstated the article must not stay behind the decoded part, or the cache is
// charged (and may refuse the part) for bytes it never uses.
func TestDecodeArticleTrimsOversizedBuffer(t *testing.T) {
	const partSize = 4096
	content := patternBytes(partSize)
	_, articles := nntptest.BuildDirectFile("movie.mkv", content, partSize)
	for _, body := range articles {
		raw := append(make([]byte, 0, 8*len(body)), body...)
		part, err := decodeArticle(raw, &yenc.InPlaceDecoder{})
		if err != nil {
			t.Fatalf("decodeArticle: %v", err)
		}
		if !bytes.Equal(part.Data, content) {
			t.Fatal("decoded data mismatch")
		}
		if c := cap(part.Data); c > 2*len(content) {
			t.Fatalf("cap(Data) = %d for %d decoded bytes: oversized buffer kept", c, len(content))
		}
	}
}

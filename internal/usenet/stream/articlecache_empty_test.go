package stream

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/usenet/yenc"
)

// onEmpty fires once, when the last source holding articles is freed — not while
// another source still has articles cached, and not while a reader holds the
// released source.
func TestArticleCacheReportsEmptyWhenLastSourceIsFreed(t *testing.T) {
	c := NewArticleCache(1 << 20)
	var emptied atomic.Int32
	c.onEmpty = func() { emptied.Add(1) }
	a, b := c.NewScope(), c.NewScope()
	for s, id := range map[*CacheScope]string{a: "a", b: "b"} {
		if _, err := s.load(context.Background(), id, func() (*yenc.Part, error) { return fixedPart(64), nil }); err != nil {
			t.Fatal(err)
		}
	}

	a.Release()
	if n := emptied.Load(); n != 0 {
		t.Fatalf("onEmpty ran %d times while another source still had articles", n)
	}
	b.retain()
	b.Release()
	if n := emptied.Load(); n != 0 {
		t.Fatalf("onEmpty ran %d times while a reader held the released source", n)
	}
	b.unretain()
	if n := emptied.Load(); n != 1 {
		t.Fatalf("onEmpty ran %d times after the last source was freed, want 1", n)
	}
	a.Release()
	b.Release()
	if n := emptied.Load(); n != 1 {
		t.Fatalf("onEmpty ran %d times after repeated releases of empty sources, want 1", n)
	}
}

package stream

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntp"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
)

// scaleTrace wraps a reader's ramp so a test can see how the window evolved.
type scaleTrace struct {
	drops, grows, maxScale int
}

// TestReadaheadRampOverSlowLink (opt-in, UNARR_USENET_RAMP_TRACE=1) streams a file
// over a simulated provider link with the adaptive window and with the base window
// pinned, and logs throughput and how the scale moved.
func TestReadaheadRampOverSlowLink(t *testing.T) {
	if os.Getenv("UNARR_USENET_RAMP_TRACE") == "" {
		t.Skip("opt-in: UNARR_USENET_RAMP_TRACE=1")
	}
	const parts, partSize = 120, 256 << 10
	content := patternBytes(parts * partSize)
	n, articles := nntptest.BuildDirectFile("movie.mkv", content, partSize)
	s := nntptest.NewFakeServer(t)
	s.AddArticles(articles)
	s.SimulateLink(1<<20, 30*time.Millisecond)
	host, port := s.Addr()
	c := nntp.NewClient(nntp.Config{Host: host, Port: port, Username: "user", Password: "pass", MaxConnections: 10})
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	f := n.Files[0]

	for _, adaptive := range []bool{false, true} {
		var fetcher ArticleFetcher = c
		if !adaptive {
			fetcher = struct{ ArticleFetcher }{c} // no pool hint: base window only
		}
		r := openReader(context.Background(), readerSource{fetcher: fetcher, ix: NewOffsetIndex(f), cache: NewArticleCache(64 << 20).NewScope()})
		var tr scaleTrace
		buf := make([]byte, 32<<10)
		start, total := time.Now(), 0
		for {
			before := r.raScale
			k, err := r.Read(buf)
			total += k
			switch {
			case r.raScale < before:
				tr.drops++
			case r.raScale > before:
				tr.grows++
			}
			tr.maxScale = max(tr.maxScale, r.raScale)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		_ = r.Close()
		secs := time.Since(start).Seconds()
		t.Logf("adaptive=%v: %.1f MiB/s over %d MiB, scale grows=%d drops=%d max=%d final=%d",
			adaptive, float64(total)/(1<<20)/secs, total>>20, tr.grows, tr.drops, tr.maxScale, r.raScale)
	}
}

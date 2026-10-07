package remotefs

import (
	"context"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntp"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
)

func TestBoundaryCatalogCancelDuringIndexing(t *testing.T) {
	server := nntptest.NewFakeServer(t)
	n, articles := nntptest.BuildDirectFile("v.mkv", make([]byte, 4096), 8192)
	server.AddArticles(articles)
	release := server.HoldBody(n.Files[0].Segments[0].MessageID)
	defer release()
	c := nntp.NewClient(server.Config())
	defer c.Close()
	dir := t.TempDir()
	writeNZB(t, dir, "v.nzb", n)
	s := &NZBSource{Directory: dir, Fetcher: c}
	cat, err := OpenCatalog(t.TempDir(), []Source{s})
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { cat.Run(ctx, time.Hour, nil); close(done) }()
	deadline := time.Now().Add(time.Second)
	for server.BodyCalls() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("indexing never requested body")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("catalog cancellation blocked on held NNTP body")
	}
	if len(cat.records["usenet"]) != 0 {
		t.Fatal("cancelled indexing published unverified metadata")
	}
}

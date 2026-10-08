package stream

import (
	"context"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntp"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
)

func TestBoundaryProbeErrorCancelsHeldSibling(t *testing.T) {
	server := nntptest.NewFakeServer(t)
	n, articles := nntptest.BuildRarStore("v.mkv", make([]byte, 16<<10), 8<<10, 16<<10)
	server.AddArticles(articles)
	// Hold every volume at mid-body. Releasing a corrupt peer must cancel them.
	var release []func()
	for _, file := range n.Files {
		release = append(release, server.HoldBody(file.Segments[0].MessageID))
	}
	defer func() {
		for _, f := range release {
			f()
		}
	}()
	id := n.Files[0].Segments[0].MessageID
	server.AddArticle(id, []byte("=ybegin line=128 size=1 name=x\r\nk\r\n"))
	cfg := server.Config()
	cfg.MaxConnections = len(n.Files)
	c := nntp.NewClient(cfg)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := Probe(ctx, c, n.RarFiles()); done <- err }()
	deadline := time.Now().Add(time.Second)
	for server.BodyCalls() < len(n.Files) {
		if time.Now().After(deadline) {
			t.Fatal("sibling bodies did not start")
		}
		time.Sleep(time.Millisecond)
	}
	release[0]()
	// Three corrupt-body attempts intentionally retain their existing two 500ms
	// backoffs. The additional 500ms allowance is for sibling cancellation.
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("corrupt probe succeeded")
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("probe error did not cancel held sibling bodies after bounded retries")
	}
}

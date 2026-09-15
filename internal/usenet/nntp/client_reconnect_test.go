package nntp_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntp"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
)

// TestCancelledCallerReconnectKeepsPoolSlot: a Body whose caller has already
// cancelled (a player closed the range while a read-ahead held a dropped
// connection) must not lose the pool slot when it reconnects. Before, the dial ran
// on the cancelled context, failed instantly, and permanently decremented the
// pool — a long-running daemon's pool shrank to empty.
func TestCancelledCallerReconnectKeepsPoolSlot(t *testing.T) {
	s := nntptest.NewFakeServer(t)
	s.AddArticle("a@test", []byte("=ybegin part=1 total=1 line=128 size=3 name=x\r\n=ypart begin=1 end=3\r\nabc\r\n=yend size=3 part=1 pcrc32=352441c2\r\n"))
	c := nntp.NewClient(s.Config())
	cctx, cancelConnect := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelConnect()
	if err := c.Connect(cctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	want := c.ActiveConnections()

	s.FailNext(1, 0) // the next BODY finds its connection dropped
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	// acquire() picks at random between a pooled connection and the cancelled
	// context, so retry until the BODY actually reached the dropped connection.
	for i := 0; i < 200 && s.BodyCalls() == 0; i++ {
		if _, err := c.Body(cancelled, "a@test"); err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled Body: %v", err)
		}
	}
	if s.BodyCalls() == 0 {
		t.Fatal("the cancelled Body never reached a connection")
	}
	if got := c.ActiveConnections(); got != want {
		t.Fatalf("ActiveConnections = %d after a cancelled reconnect, want %d", got, want)
	}
	if _, err := c.Body(context.Background(), "a@test"); err != nil {
		t.Fatalf("Body after cancelled reconnect: %v", err)
	}
}

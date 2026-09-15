package nntp_test

// A redial that the provider refuses must not fail a caller while other
// connections are live and will be released, and must not hang one when nothing
// live remains.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntp"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
)

// loseConnectionToStall breaks one pooled connection with a BODY the provider
// never answers. The client closes it, but the provider keeps counting it, so
// under LimitConnections the replacement dial is refused and the slot is lost.
func loseConnectionToStall(t *testing.T, c *nntp.Client, s *nntptest.FakeServer) {
	t.Helper()
	s.StallArticle("stuck@test")
	before := c.ActiveConnections()
	ctx := nntp.WithStallTimeout(context.Background(), 100*time.Millisecond)
	if _, err := c.Body(ctx, "stuck@test"); err == nil {
		t.Fatal("Body of a stalled article succeeded")
	}
	if got := c.ActiveConnections(); got != before-1 {
		t.Fatalf("ActiveConnections = %d after the refused replacement, want %d", got, before-1)
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// The provider caps connections at MaxConnections and still counts one the client
// retired, so a redial is refused while the only live connection is busy. Body must
// wait for that connection to be released instead of failing the article, and must
// not spin on the refused dial while it waits.
func TestRefusedRedialWaitsForBusyConnection(t *testing.T) {
	s := nntptest.NewFakeServer(t)
	s.AddArticle("a@test", article("a", []byte("busy")))
	s.AddArticle("b@test", article("b", []byte("waited")))
	s.LimitConnections(2)
	c := dialPool(t, s, 2)
	loseConnectionToStall(t, c, s)

	s.DelayNext(1, 600*time.Millisecond)
	calls := s.BodyCalls()
	busy := make(chan error, 1)
	go func() {
		_, err := c.Body(context.Background(), "a@test")
		busy <- err
	}()
	waitUntil(t, "the busy BODY to reach the server", func() bool { return s.BodyCalls() > calls })

	accepted := s.Connections()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := c.Body(ctx, "b@test")
	if err != nil {
		t.Fatalf("Body with a refused redial and a busy live connection: %v; want it to wait for the release", err)
	}
	if s := decoded(t, got); s != "waited" {
		t.Fatalf("got %q", s)
	}
	if n := s.Connections() - accepted; n > 2 {
		t.Fatalf("%d dials while waiting ~600 ms for the release, want at most 2 (no dial loop)", n)
	}
	if err := <-busy; err != nil {
		t.Fatalf("busy Body: %v", err)
	}
}

// With no live connection left there is no release to wait for: a refused redial
// must fail the call promptly rather than hang until the caller's deadline.
func TestRefusedRedialWithNothingLiveFailsPromptly(t *testing.T) {
	s := nntptest.NewFakeServer(t)
	s.AddArticle("a@test", article("a", []byte("unreachable")))
	s.LimitConnections(1)
	c := dialPool(t, s, 1)
	loseConnectionToStall(t, c, s)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err := c.Body(ctx, "a@test")
	if err == nil {
		t.Fatal("Body succeeded although every dial is refused")
	}
	if errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("Body took %v (err %v): it waited for a release no live connection can make", time.Since(start), err)
	}
	if got := c.ActiveConnections(); got != 0 {
		t.Fatalf("ActiveConnections = %d, want 0", got)
	}
}

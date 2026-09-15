package nntp_test

import (
	"context"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntp"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
)

// TestStallBoundCutsAStalledBody: under WithStallTimeout a server that never
// answers BODY for an article costs two bounds — the pooled connection, then one
// retry on a fresh connection proven alive — not the 60 s command deadline twice,
// and only then is it reported as a stall. The pool recovers for the next Body.
func TestStallBoundCutsAStalledBody(t *testing.T) {
	s := nntptest.NewFakeServer(t)
	s.AddArticle("a@test", tinyArticle)
	s.StallArticle("slow@test")
	c := dialTest(t, s)

	ctx := nntp.WithStallTimeout(context.Background(), 150*time.Millisecond)
	start := time.Now()
	_, err := c.Body(ctx, "slow@test")
	elapsed := time.Since(start)
	if err == nil || !nntp.IsStalled(err) {
		t.Fatalf("err = %v, want a stall", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("stalled BODY took %s, want about two 150ms bounds", elapsed)
	}
	if got := s.BodyCalls(); got != 2 {
		t.Fatalf("stalled BODY issued %d times, want 2 (pooled, then one fresh retry)", got)
	}
	if _, err := c.Body(ctx, "a@test"); err != nil {
		t.Fatalf("Body after a stall: %v", err)
	}
}

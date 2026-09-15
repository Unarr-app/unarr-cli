package nntp_test

// Connection-lifecycle invariants of the pool: a connection whose state is
// unknown is never reused, every connection that leaves frees its slot exactly
// once, and only a stall on a connection proven alive is reported as a stall.

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntp"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
	"github.com/Unarr-app/unarr-cli/internal/usenet/yenc"
)

// article returns a one-part yEnc body carrying data.
func article(name string, data []byte) []byte {
	return yenc.Encode(name, 1, 1, 1, int64(len(data)), int64(len(data)), data)
}

// decoded yEnc-decodes a BODY result.
func decoded(t *testing.T, raw []byte) string {
	t.Helper()
	p, err := yenc.DecodeBytes(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return string(p.Data)
}

// dialPool connects a client with max connections (0: the fake server's default).
func dialPool(t *testing.T, s *nntptest.FakeServer, max int) *nntp.Client {
	t.Helper()
	cfg := s.Config()
	if max > 0 {
		cfg.MaxConnections = max
	}
	c := nntp.NewClient(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// A pooled connection silently dropped while idle accepts BODY and never
// answers. Under the stall bound that is a timeout on a DEAD connection, not a
// verdict on the article: Body must retry on a fresh connection and succeed.
func TestTimeoutOnPooledConnectionIsRetriedOnFreshConnection(t *testing.T) {
	s := nntptest.NewFakeServer(t)
	s.AddArticle("a@test", article("a", []byte("alive")))
	c := dialPool(t, s, 2)

	s.StallNext(1)
	ctx := nntp.WithStallTimeout(context.Background(), 150*time.Millisecond)
	got, err := c.Body(ctx, "a@test")
	if err != nil {
		t.Fatalf("Body after a dead pooled connection: %v (IsStalled=%v); want a retry on a fresh connection", err, nntp.IsStalled(err))
	}
	if decoded(t, got) != "alive" {
		t.Fatalf("got %q", decoded(t, got))
	}
	if n := s.BodyCalls(); n != 2 {
		t.Fatalf("BodyCalls = %d, want 2 (dead pooled connection, then the fresh one)", n)
	}
}

// A connection whose BODY timed out may still receive that reply. Returned to
// the pool, the next Body on it would read the late reply as its own article.
func TestTimedOutConnectionIsNeverReused(t *testing.T) {
	s := nntptest.NewFakeServer(t)
	s.AddArticle("a@test", article("a", []byte("article A")))
	s.AddArticle("b@test", article("b", []byte("article B")))
	c := dialPool(t, s, 1)

	s.FailNext(1, 0)                     // the pooled connection drops: Body redials
	s.DelayNext(1, 400*time.Millisecond) // and the fresh connection's answer comes late
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := c.Body(ctx, "a@test"); err == nil {
		t.Fatal("Body succeeded although its answer came after the deadline")
	}

	got, err := c.Body(context.Background(), "b@test")
	if err != nil {
		t.Fatalf("Body(b): %v", err)
	}
	if s := decoded(t, got); s != "article B" {
		t.Fatalf("Body(b) returned %q: the late reply to a timed-out BODY was read as the next article", s)
	}
}

// A transfer reset mid-article breaks the connection. Every such connection must
// leave the open count, or a pool of dead slots blocks every Body in acquire
// forever. Once all slots are gone, Body must redial rather than hang.
func TestMidBodyResetsNeverLeakPoolSlots(t *testing.T) {
	s := nntptest.NewFakeServer(t)
	s.AddArticle("a@test", article("a", []byte("survives")))
	c := dialPool(t, s, 2)

	s.ResetMidBodyNext(4) // both attempts of two Body calls are cut mid-article
	for i := 0; i < 2; i++ {
		if _, err := c.Body(context.Background(), "a@test"); err == nil {
			t.Fatalf("Body %d succeeded through two mid-body resets", i+1)
		}
	}
	if got := c.ActiveConnections(); got != 0 {
		t.Fatalf("ActiveConnections = %d after every connection was reset mid-body, want 0 (slots leaked)", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := c.Body(ctx, "a@test")
	if err != nil {
		t.Fatalf("Body on an emptied pool: %v (want a redial, not a hang)", err)
	}
	if decoded(t, got) != "survives" {
		t.Fatalf("got %q", decoded(t, got))
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// Connection-level timeouts are transport errors, not article stalls: they must
// never be memoised against the article or answered as 504 + missing-article.
func TestConnectionTimeoutsAreNotStalls(t *testing.T) {
	cases := map[string]error{
		"dial timeout in reconnect": fmt.Errorf("nntp: body failed and reconnect failed: %w",
			&net.OpError{Op: "dial", Net: "tcp", Err: timeoutError{}}),
		"body transfer timeout after 222": fmt.Errorf("read body: %w",
			&net.OpError{Op: "read", Net: "tcp", Err: timeoutError{}}),
		"redial timeout in acquire": fmt.Errorf("nntp: redial: %w",
			&net.OpError{Op: "dial", Net: "tcp", Err: timeoutError{}}),
	}
	for name, err := range cases {
		if nntp.IsStalled(err) {
			t.Errorf("%s: IsStalled = true, want false (a transport error, not an article stall)", name)
		}
	}
}

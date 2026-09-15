package nntp_test

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntp"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
)

// tinyArticle is a valid one-part yEnc body for tests that only need BODY to succeed.
var tinyArticle = []byte("=ybegin part=1 total=1 line=128 size=3 name=x\r\n=ypart begin=1 end=3\r\nabc\r\n=yend size=3 part=1 pcrc32=352441c2\r\n")

// dialTest connects a client to s for the duration of the test.
func dialTest(t *testing.T, s *nntptest.FakeServer) *nntp.Client {
	t.Helper()
	return dialPool(t, s, 0)
}

// TestArticleMissingIsFinalWithoutReconnect: 430 (and 423) is a complete answer
// on a healthy connection. It must cost exactly one BODY — no reconnect, no
// re-ask on a fresh connection — and leave the connection usable.
func TestArticleMissingIsFinalWithoutReconnect(t *testing.T) {
	for _, code := range []int{430, 423} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			s := nntptest.NewFakeServer(t)
			s.AddArticle("a@test", tinyArticle)
			c := dialTest(t, s)
			conns, active := s.Connections(), c.ActiveConnections()

			s.FailNext(1, code)
			_, err := c.Body(context.Background(), "a@test")
			var nf *nntp.ArticleNotFoundError
			if !errors.As(err, &nf) {
				t.Fatalf("err = %v, want *ArticleNotFoundError", err)
			}
			if got := s.BodyCalls(); got != 1 {
				t.Fatalf("%d cost %d BODY, want exactly 1", code, got)
			}
			if got := s.Connections(); got != conns {
				t.Fatalf("%d redialled: %d connections accepted, want %d", code, got, conns)
			}
			if got := c.ActiveConnections(); got != active {
				t.Fatalf("ActiveConnections = %d after %d, want %d", got, code, active)
			}
			if _, err := c.Body(context.Background(), "a@test"); err != nil {
				t.Fatalf("Body after %d: %v", code, err)
			}
		})
	}
}

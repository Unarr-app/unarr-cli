package metrics

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// rawResult is what rawGet saw: the status and whether the server asked to
// close the connection.
type rawResult struct {
	status int
	close  bool
}

// rawGet sends one keep-alive GET /metrics over a fresh connection and reads the
// response, leaving the connection open for the caller to watch.
func rawGet(t *testing.T, addr, extraHeaders string) (net.Conn, *bufio.Reader, rawResult) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	req := "GET /metrics HTTP/1.1\r\nHost: x\r\n" + extraHeaders + "\r\n"
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return c, br, rawResult{status: resp.StatusCode, close: resp.Close}
}

// closedByServer waits (bounded) for the server to close c.
func closedByServer(t *testing.T, c net.Conn, br *bufio.Reader) bool {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := br.ReadByte()
	return errors.Is(err, io.EOF)
}

// An idle keep-alive connection must not be held forever: a few hundred of them
// were enough to pin the endpoint's goroutines and fds.
func TestMetricsClosesIdleKeepAlive(t *testing.T) {
	prev := idleTimeout
	idleTimeout = 50 * time.Millisecond
	t.Cleanup(func() { idleTimeout = prev })

	srv, _ := startTestServer(t, "tok")
	c, br, res := rawGet(t, srv.Addr(), "Authorization: Bearer tok\r\n")
	if res.status != http.StatusOK || res.close {
		t.Fatalf("status %d close=%v, want a 200 kept alive", res.status, res.close)
	}
	if !closedByServer(t, c, br) {
		t.Fatal("idle keep-alive connection still open past the idle timeout")
	}
}

// A rejected request gets its connection closed at once: an unauthenticated
// client has no business keeping one open.
func TestMetricsUnauthorizedClosesConnection(t *testing.T) {
	srv, _ := startTestServer(t, "tok")
	c, br, res := rawGet(t, srv.Addr(), "")
	if res.status != http.StatusUnauthorized || !res.close {
		t.Fatalf("status %d close=%v, want 401 with Connection: close", res.status, res.close)
	}
	if !closedByServer(t, c, br) {
		t.Fatal("connection still open after a 401")
	}
}

func TestMetricsRejectsHugeHeaders(t *testing.T) {
	srv, _ := startTestServer(t, "tok")
	_, _, res := rawGet(t, srv.Addr(), "X-Junk: "+strings.Repeat("a", 64<<10)+"\r\n")
	if res.status != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("status %d, want 431", res.status)
	}
}

package control

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// An idle keep-alive connection to the control plane must be closed by the
// server: with only ReadHeaderTimeout set it was held open indefinitely.
func TestServerClosesIdleKeepAlive(t *testing.T) {
	prev := idleTimeout
	idleTimeout = 50 * time.Millisecond
	t.Cleanup(func() { idleTimeout = prev })

	srv, _ := startTestServer(t, &fakeController{})
	c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", srv.Port()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	req := "GET /tasks HTTP/1.1\r\nHost: x\r\n" + TokenHeader + ": " + srv.Token() + "\r\n\r\n"
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
	if resp.StatusCode != http.StatusOK || resp.Close {
		t.Fatalf("status %d close=%v, want a 200 kept alive", resp.StatusCode, resp.Close)
	}

	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
		t.Fatalf("idle connection not closed by the server: %v", err)
	}
}

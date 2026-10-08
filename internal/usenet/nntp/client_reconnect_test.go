package nntp_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntp"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
)

// A caller cancelled while holding a stalled connection must retire it and
// leave its slot available to the next live caller. Wait for the BODY to reach
// the server before cancelling: an already-cancelled context can close the
// socket before the command is written, independently of pool acquisition.
func TestCancelledCallerReconnectKeepsPoolSlot(t *testing.T) {
	s := nntptest.NewFakeServer(t)
	s.AddArticle("a@test", article("a", []byte("recovered")))
	cfg := s.Config()
	cfg.MaxConnections = 1 // Recovery must refill the retired slot, not use another.
	c := nntp.NewClient(cfg)
	cctx, cancelConnect := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelConnect()
	if err := c.Connect(cctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	want := c.ActiveConnections()

	s.StallNext(1)
	cancelled, cancel := context.WithCancel(cctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.Body(cancelled, "a@test")
		done <- err
	}()
	waitUntil(t, "the cancelled BODY to reach the server", func() bool { return s.BodyCalls() == 1 })
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled Body: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled Body did not release its connection")
	}
	if got := s.BodyCalls(); got != 1 {
		t.Fatalf("cancelled Body retried: %d commands, want 1", got)
	}
	if got := c.ActiveConnections(); got > want {
		t.Fatalf("ActiveConnections = %d after cancellation, exceeds %d slots", got, want)
	}
	gotBody, err := c.Body(cctx, "a@test")
	if err != nil {
		t.Fatalf("Body after cancelled reconnect: %v", err)
	}
	if got := decoded(t, gotBody); got != "recovered" {
		t.Fatalf("Body after cancelled reconnect = %q, want recovered", got)
	}
	// Cancellation now retires promptly; the next live owner can redial the
	// vacated slot rather than forcing the cancelled owner through a handshake.
	for range want {
		if _, err := c.Body(cctx, "a@test"); err != nil {
			t.Fatal(err)
		}
	}
	if got := c.ActiveConnections(); got < 1 || got > want {
		t.Fatalf("usable pool after cancellation = %d", got)
	}
}

type countingProgress struct {
	lines, restarts, linesAtRestart int
	last                            []byte
}

func TestAlreadyCancelledBodyKeepsPoolCapacity(t *testing.T) {
	s := nntptest.NewFakeServer(t)
	s.AddArticle("a@test", article("a", []byte("recovered")))
	c := dialPool(t, s, 1)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Body(cancelled, "a@test"); !errors.Is(err, context.Canceled) {
		t.Fatalf("already-cancelled Body: %v", err)
	}
	ctx, cancelLive := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelLive()
	got, err := c.Body(ctx, "a@test")
	if err != nil {
		t.Fatalf("Body after already-cancelled caller: %v", err)
	}
	if decoded(t, got) != "recovered" || c.ActiveConnections() != 1 {
		t.Fatalf("already-cancelled caller lost usable capacity: %d connections", c.ActiveConnections())
	}
}

func (p *countingProgress) Line(body []byte) { p.lines++; p.last = body }
func (p *countingProgress) Restart()         { p.restarts++; p.linesAtRestart = p.lines }

// A body cut mid-transfer is read again on a new connection into the same
// buffer: the lines reported before must be declared void first, or whoever
// used them would see them overwritten.
func TestBodyProgressRestartsWithTheRetry(t *testing.T) {
	s := nntptest.NewFakeServer(t)
	var body []byte
	for i := 0; i < 400; i++ {
		body = append(body, "0123456789abcdefghijklmnopqrstuvwxyz0123456789\r\n"...)
	}
	s.AddArticle("cut@test", body)
	c := nntp.NewClient(s.Config())
	cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(cctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	s.ResetMidBodyNext(1)
	p := &countingProgress{}
	got, err := c.BodyInto(nntp.WithBodyProgress(cctx, p), "cut@test", make([]byte, 0, 64<<10))
	if err != nil {
		t.Fatalf("BodyInto: %v", err)
	}
	if p.restarts != 1 || p.linesAtRestart == 0 {
		t.Fatalf("restarts = %d after %d lines, want 1 after some lines of the cut body", p.restarts, p.linesAtRestart)
	}
	if lines := strings.Count(string(got), "\n"); p.lines-p.linesAtRestart != lines || string(p.last) != string(got) {
		t.Fatalf("retry reported %d lines ending in %d bytes, want %d ending in the %d-byte body",
			p.lines-p.linesAtRestart, len(p.last), lines, len(got))
	}
}

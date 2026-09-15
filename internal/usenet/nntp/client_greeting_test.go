package nntp

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

// A server (or a TLS proxy in front of one) that accepts a connection and never
// sends its greeting must not hold a dial forever: the Body that dials would never
// return, and its reader, handler goroutine and pool slot would leak with it.
func TestDialTimesOutOnServerThatNeverGreets(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, conn) // accepted, never greeted
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range held {
			conn.Close()
		}
	})

	c := NewClient(Config{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, MaxConnections: 1})
	c.handshakeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { _ = c.Close() })

	result := make(chan error, 1)
	go func() {
		_, err := c.Body(context.Background(), "a@test")
		result <- err
	}()
	select {
	case err := <-result:
		if !isNetTimeout(err) {
			t.Fatalf("Body err = %v, want a connection timeout", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Body still dialling 3 s after a 200 ms handshake bound: a silent server holds the dial forever")
	}
	if got := c.ActiveConnections(); got != 0 {
		t.Fatalf("ActiveConnections = %d after the failed dial, want 0", got)
	}
}

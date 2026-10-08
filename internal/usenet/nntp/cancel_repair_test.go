package nntp

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestBoundaryCancelDuringRepairHandshake(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	repair := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		first, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = io.WriteString(first, "200 fixture\r\n")
		_, _ = bufio.NewReader(first).ReadString('\n')
		_ = first.Close() // first BODY fails, forcing a retry
		second, err := ln.Accept()
		if err != nil {
			return
		}
		defer second.Close()
		close(repair) // accepted but never greeted: the old repair could take 30s
		_, _ = io.Copy(io.Discard, second)
	}()
	t.Cleanup(func() { _ = ln.Close(); wg.Wait() })
	c := NewClient(Config{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, MaxConnections: 1})
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.Body(ctx, "a@test"); done <- err }()
	select {
	case <-repair:
	case <-time.After(time.Second):
		t.Fatal("repair did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("repair handshake delayed cancellation")
	}
	if c.ActiveConnections() != 0 {
		t.Fatal("cancelled repair kept its slot")
	}
}

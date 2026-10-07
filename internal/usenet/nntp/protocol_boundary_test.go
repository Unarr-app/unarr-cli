package nntp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Local wire fixture: no external provider, bounded lifetime, exact command capture.
func boundaryServer(t *testing.T, body func(net.Conn, *bufio.Writer, string)) (Config, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var commands, accepted atomic.Int32
	var mu sync.Mutex
	var sockets []net.Conn
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			cn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			mu.Lock()
			sockets = append(sockets, cn)
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer cn.Close()
				_ = cn.SetDeadline(time.Now().Add(5 * time.Second))
				w := bufio.NewWriter(cn)
				_, _ = w.WriteString("200 local fixture\r\n")
				_ = w.Flush()
				r := bufio.NewScanner(cn)
				for r.Scan() {
					line := r.Text()
					switch {
					case strings.HasPrefix(line, "AUTHINFO USER "):
						_, _ = w.WriteString("381 password\r\n")
						_ = w.Flush()
					case strings.HasPrefix(line, "AUTHINFO PASS "):
						_, _ = w.WriteString("281 ok\r\n")
						_ = w.Flush()
					case line == "QUIT":
						return
					default:
						commands.Add(1)
						body(cn, w, line)
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, cn := range sockets {
			_ = cn.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	a := ln.Addr().(*net.TCPAddr)
	return Config{Host: "127.0.0.1", Port: a.Port, Username: "user", Password: "pass", MaxConnections: 1}, &commands, &accepted
}

func TestBoundaryMessageIDBeforeCommands(t *testing.T) {
	for _, id := range []string{"x@test>\r\nDATE\r\nBODY <y@test", "x\ry@test", "x\ny@test", "<x@test", "x@test>", "x<y@test", "x y@test", "x\x00@test"} {
		t.Run(fmt.Sprintf("%q", id), func(t *testing.T) {
			cfg, commands, _ := boundaryServer(t, func(_ net.Conn, w *bufio.Writer, _ string) {
				_, _ = w.WriteString("222 body\r\nk\r\n.\r\n")
				_ = w.Flush()
			})
			c := NewClient(cfg)
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := c.Connect(ctx); err != nil {
				t.Fatal(err)
			}
			_, err := c.Body(ctx, id)
			if err == nil || commands.Load() != 0 {
				t.Fatalf("invalid ID: err=%v, wire commands=%d; want validation before wire", err, commands.Load())
			}
		})
	}
}

func TestBoundaryCredentialsBeforeDial(t *testing.T) {
	for _, password := range []bool{false, true} {
		cfg, _, accepted := boundaryServer(t, func(_ net.Conn, w *bufio.Writer, _ string) { _, _ = w.WriteString("500 invalid\r\n"); _ = w.Flush() })
		if password {
			cfg.Password = "p\r\nDATE"
		} else {
			cfg.Username = "u\nDATE"
		}
		c := NewClient(cfg)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := c.Connect(ctx)
		cancel()
		_ = c.Close()
		if err == nil || accepted.Load() != 0 {
			t.Errorf("invalid credentials: err=%v, connections=%d", err, accepted.Load())
		}
	}
}

// Existing API baseline: intentionally uses the public operation and real socket.
func TestBoundaryCancelInFlight(t *testing.T) {
	started := make(chan struct{}, 1)
	cfg, _, _ := boundaryServer(t, func(cn net.Conn, w *bufio.Writer, line string) {
		_, _ = w.WriteString("222 body\r\n")
		_ = w.Flush()
		if line == "BODY <stall@test>" {
			started <- struct{}{}
			_, _ = io.Copy(io.Discard, cn)
			return
		}
		_, _ = w.WriteString("healthy\r\n.\r\n")
		_ = w.Flush()
	})
	c := NewClient(cfg)
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.Body(ctx, "stall@test"); done <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("BODY never started")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Error("cancel did not interrupt body within 250ms")
		return
	}
	next, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	got, err := c.Body(next, "healthy@test")
	if err != nil || string(got) != "healthy\n" {
		t.Fatalf("pool recovery: %q, %v", got, err)
	}
}

func TestBoundaryEncodedBodyCeiling(t *testing.T) {
	for _, short := range []bool{false, true} {
		input := strings.Repeat("x", (16<<20)+1) + "\r\n.\r\n"
		if short {
			input = strings.Repeat("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\r\n", (16<<20)/32+1) + ".\r\n"
		}
		got, err := readDotBody(bufio.NewReader(strings.NewReader(input)), nil, nil)
		if err == nil {
			t.Errorf("short=%v accepted %d encoded bytes past 16MiB ceiling", short, len(got))
		}
	}
}

func TestBoundaryRequiresDot(t *testing.T) {
	for _, input := range []string{"=ybegin line=128 size=1 name=v.mkv\r\nk\r\n", "whole\r\npartial"} {
		_, err := readDotBody(bufio.NewReader(strings.NewReader(input)), nil, nil)
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("missing dot: %v", err)
		}
	}
}

func TestBoundarySmallReceiveLimits(t *testing.T) {
	for _, pooled := range []bool{false, true} {
		for _, n := range []int{58, 59, 60} {
			buf := []byte(nil)
			if pooled {
				buf = make([]byte, 0, 64)
			}
			// Data plus CRLF plus dot terminator: 63, 64 and 65 wire bytes.
			got, err := readDotBodyLimit(bufio.NewReaderSize(strings.NewReader(strings.Repeat("x", n)+"\r\n.\r\n"), 16), buf, nil, 64)
			if n <= 59 && (err != nil || len(got) != n+1 || cap(got) > 64) {
				t.Errorf("pooled=%v n=%d: len/cap=%d/%d err=%v", pooled, n, len(got), cap(got), err)
			}
			var oversized *BodyTooLargeError
			if n == 60 && !errors.As(err, &oversized) {
				t.Errorf("limit+1: %v", err)
			}
		}
	}
	for _, input := range []string{strings.Repeat("xxxxxxxxxxxxxx\r\n", 10), strings.Repeat("x", 1000)} {
		b := bodyReceiver{reader: bufio.NewReaderSize(strings.NewReader(input), 16), limit: 64}
		var err error
		for err == nil {
			err = b.appendLine()
		}
		var oversized *BodyTooLargeError
		if !errors.As(err, &oversized) || cap(b.out) > 64 || len(b.out) > 64 || b.received > 64 {
			t.Fatalf("receive storage exceeded bound: len/cap/received=%d/%d/%d err=%v", len(b.out), cap(b.out), b.received, err)
		}
	}
}

func TestBoundaryOversizeRetiresAndRecovers(t *testing.T) {
	for _, pooled := range []bool{false, true} {
		cfg, commands, accepted := boundaryServer(t, func(_ net.Conn, w *bufio.Writer, line string) {
			_, _ = w.WriteString("222 body\r\n")
			if line == "BODY <big@test>" {
				_, _ = w.WriteString(strings.Repeat("x", 1000) + "\r\n.\r\n")
			} else {
				_, _ = w.WriteString("healthy\r\n.\r\n")
			}
			_ = w.Flush()
		})
		c := NewClient(cfg)
		c.encodedBodyLimit = 64
		defer c.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		buf := []byte(nil)
		if pooled {
			buf = make([]byte, 0, 32)
		}
		_, err := c.BodyInto(ctx, "big@test", buf)
		var oversized *BodyTooLargeError
		if !errors.As(err, &oversized) || c.ActiveConnections() != 0 || commands.Load() != 1 {
			t.Fatalf("oversize err=%v open=%d commands=%d", err, c.ActiveConnections(), commands.Load())
		}
		got, err := c.Body(ctx, "healthy@test")
		if err != nil || string(got) != "healthy\n" || accepted.Load() != 2 {
			t.Fatalf("retired socket reused/lost slot: %q %v accepted=%d", got, err, accepted.Load())
		}
	}
}

func TestBoundaryValidIDAndStoppedCallback(t *testing.T) {
	cfg, _, accepted := boundaryServer(t, func(_ net.Conn, w *bufio.Writer, line string) {
		if line != "BODY <valid@test>" {
			t.Errorf("unexpected valid command %q", line)
		}
		_, _ = w.WriteString("222 body\r\nhealthy\r\n.\r\n")
		_ = w.Flush()
	})
	c := NewClient(cfg)
	defer c.Close()
	for range 30 {
		ctx, cancel := context.WithCancel(context.Background())
		if _, err := c.Body(ctx, "<valid@test>"); err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel() // must not close the socket after its operation released it
		if _, err := c.Body(context.Background(), "valid@test"); err != nil {
			t.Fatal(err)
		}
	}
	if accepted.Load() != 1 {
		t.Fatalf("late callback retired a new owner: %d sockets", accepted.Load())
	}
}

func TestBoundaryIncompleteWireRetiresAndRetries(t *testing.T) {
	cfg, commands, accepted := boundaryServer(t, func(cn net.Conn, w *bufio.Writer, line string) {
		_, _ = w.WriteString("222 body\r\n=ybegin line=128 size=1 name=v.mkv\r\nk\r\n=yend size=1\r\n")
		if line == "BODY <cut@test>" {
			_ = w.Flush()
			_ = cn.Close()
			return
		}
		_, _ = w.WriteString(".\r\n")
		_ = w.Flush()
	})
	c := NewClient(cfg)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := c.Body(ctx, "cut@test"); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("incomplete wire succeeded: %v", err)
	}
	if c.ActiveConnections() != 0 || commands.Load() != 2 {
		t.Fatalf("incomplete wire slot/attempts: open=%d commands=%d", c.ActiveConnections(), commands.Load())
	}
	if _, err := c.Body(ctx, "healthy@test"); err != nil {
		t.Fatal(err)
	}
	if accepted.Load() != 3 {
		t.Fatal("incomplete connection reused")
	}
}

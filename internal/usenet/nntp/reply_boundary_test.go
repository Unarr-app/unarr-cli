package nntp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Uses the existing public Connect/BODY APIs before the reply-bound fix.
func TestFinalOversizedReplyPublicAPI(t *testing.T) {
	for _, phase := range []string{"greeting", "auth-user", "auth-pass", "body"} {
		t.Run(phase, func(t *testing.T) {
			// The greeting and BODY reproduce Astra's real 16 MiB + 128 text
			// probe; auth also exceeds the proposed generous 64 KiB reply cap.
			size := (64 << 10) + 1
			if phase == "greeting" || phase == "body" {
				size = MaxEncodedBodyBytes + 128 + 6
			}
			cfg, accepted := finalReplyServer(t, phase, size, false)
			client := NewClient(cfg)
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var err error
			if phase == "body" {
				_, err = client.Body(ctx, "large@test")
			} else {
				err = client.Connect(ctx)
			}
			if err == nil {
				t.Fatalf("accepted %d-byte %s reply", size, phase)
			}
			if ctx.Err() != nil || IsArticleMissing(err) || client.ActiveConnections() != 0 || accepted.Load() != 1 {
				t.Fatalf("oversize retirement: err=%v ctx=%v open=%d sockets=%d", err, ctx.Err(), client.ActiveConnections(), accepted.Load())
			}
			body, err := client.Body(ctx, "healthy@test")
			if err != nil || string(body) != "payload\n" || accepted.Load() != 2 {
				t.Fatalf("recovered pool slot/body framing: %q %v sockets=%d", body, err, accepted.Load())
			}
		})
	}
}

func TestFinalReplyCeilingPublicAPI(t *testing.T) {
	for _, size := range []int{(64 << 10) - 1, 64 << 10, (64 << 10) + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			cfg, _ := finalReplyServer(t, "body", size, false)
			client := NewClient(cfg)
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			body, err := client.Body(ctx, "boundary@test")
			if size <= 64<<10 {
				if err != nil || string(body) != "payload\n" {
					t.Fatalf("valid boundary reply/body: %q %v", body, err)
				}
			} else if err == nil || client.ActiveConnections() != 0 {
				t.Fatalf("reply limit+1 accepted/reused: %q %v", body, err)
			}
		})
	}
}

func TestFinalReplyContinuationsCannotBeCleanAlternatives(t *testing.T) {
	for _, phase := range []string{"greeting", "auth-user", "body"} {
		t.Run(phase, func(t *testing.T) {
			// Many short lines total >64 KiB. NNTP status is one line:
			// rejected continuation must never be a clean 201/281/430 path.
			cfg, accepted := finalReplyServer(t, phase, 2048, true)
			if phase == "greeting" {
				cfg.Username, cfg.Password = "", ""
			}
			client := NewClient(cfg)
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var err error
			if phase == "body" {
				body, err := client.Body(ctx, "continued@test")
				if err != nil || string(body) != "payload\n" || accepted.Load() != 2 {
					t.Fatalf("invalid 430 continuation was cached as missing/reused: %q %v sockets=%d", body, err, accepted.Load())
				}
				return
			}
			err = client.Connect(ctx)
			if err == nil || ctx.Err() != nil || client.ActiveConnections() != 0 {
				t.Fatalf("invalid alternative status accepted: err=%v ctx=%v open=%d", err, ctx.Err(), client.ActiveConnections())
			}
			if _, err := client.Body(ctx, "healthy@test"); err != nil || accepted.Load() != 2 {
				t.Fatal("continuation retirement lost slot", err)
			}
		})
	}
}

func TestFinalReplyFramingAndBufferedBytes(t *testing.T) {
	for _, code := range []string{"200", "201", "281", "381", "222", "430", "423"} {
		reader, writer := net.Pipe()
		go func() {
			defer writer.Close()
			_, _ = io.WriteString(writer, code+" ok\r\npayload\r\n.\r\n")
		}()
		tp := textproto.NewConn(reader)
		got, message, err := readReply(tp, 222)
		if fmt.Sprint(got) != code || message != "ok" || (err == nil) != (code == "222") {
			t.Errorf("valid complete code %s changed: %d %q %v", code, got, message, err)
		}
		body, bodyErr := readDotBody(tp.R, nil, nil)
		_ = tp.Close()
		if bodyErr != nil || string(body) != "payload\n" {
			t.Fatalf("buffered following bytes corrupted: %q %v", body, bodyErr)
		}
	}
	for _, input := range []string{"201 incomplete", "281-no password\r\n", "430-missing\r\n", "222\r\n", "x22 invalid\r\n"} {
		reader, writer := net.Pipe()
		go func() {
			defer writer.Close()
			_, _ = io.WriteString(writer, input)
		}()
		tp := textproto.NewConn(reader)
		code, _, err := readReply(tp, 222)
		_ = tp.Close()
		if err == nil || code != 0 {
			t.Errorf("incomplete/invalid framing exposed a clean code: %q %d %v", input, code, err)
		}
	}
}

func finalReplyServer(t *testing.T, phase string, wireSize int, continuation bool) (Config, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	var mu sync.Mutex
	var sockets []net.Conn
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			cn, err := listener.Accept()
			if err != nil {
				return
			}
			bad := accepted.Add(1) == 1
			mu.Lock()
			sockets = append(sockets, cn)
			mu.Unlock()
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer cn.Close()
				_ = cn.SetDeadline(time.Now().Add(4 * time.Second))
				finalServeReplies(cn, phase, wireSize, bad, continuation)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		for _, cn := range sockets {
			_ = cn.Close()
		}
		mu.Unlock()
		workers.Wait()
	})
	return Config{Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, Username: "user", Password: "pass", MaxConnections: 1}, &accepted
}

func finalServeReplies(cn net.Conn, phase string, wireSize int, bad, continuation bool) {
	reply := func(at, code string) error {
		if !bad || phase != at {
			_, err := io.WriteString(cn, code+" ok\r\n")
			return err
		}
		if continuation {
			switch at {
			case "greeting":
				code = "201"
			case "auth-user":
				code = "281"
			case "body":
				code = "430"
			}
			for range 40 {
				if _, err := io.WriteString(cn, code+"-"+strings.Repeat("x", wireSize-6)+"\r\n"); err != nil {
					return err
				}
			}
			_, err := io.WriteString(cn, code+" end\r\n")
			return err
		}
		if _, err := io.WriteString(cn, code+" "); err != nil {
			return err
		}
		chunk := strings.Repeat("x", 4096)
		for remaining := wireSize - 6; remaining > 0; {
			n := min(remaining, len(chunk))
			if _, err := io.WriteString(cn, chunk[:n]); err != nil {
				return err
			}
			remaining -= n
		}
		_, err := io.WriteString(cn, "\r\n")
		return err
	}
	if err := reply("greeting", "200"); err != nil {
		return
	}
	scanner := bufio.NewScanner(cn)
	for scanner.Scan() {
		line := scanner.Text()
		var err error
		switch {
		case strings.HasPrefix(line, "AUTHINFO USER "):
			err = reply("auth-user", "381")
		case strings.HasPrefix(line, "AUTHINFO PASS "):
			err = reply("auth-pass", "281")
		case strings.HasPrefix(line, "BODY "):
			err = reply("body", "222")
			if err == nil {
				_, err = io.WriteString(cn, "payload\r\n.\r\n")
			}
		default:
			err = errors.New("unexpected fixture command")
		}
		if err != nil {
			return
		}
	}
}

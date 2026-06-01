package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseServer returns an httptest server that writes the given raw SSE body and
// flushes, then holds the connection until the request context is cancelled (so
// the client drives the close, like the real long-lived endpoint).
func sseServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(body)); err != nil {
			return
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
}

func TestOpenEventStream_ParsesTypedEvents(t *testing.T) {
	body := "retry: 2000\n\n" +
		": connected hb=15000\n\n" +
		"event: sync\ndata: {\"reason\":\"wake\"}\n\n" +
		"event: command\ndata: {\"controls\":[{\"action\":\"cancel\",\"taskId\":\"t1\"}]}\n\n"
	srv := sseServer(t, body)
	defer srv.Close()

	_, client := newTestSyncClient(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := client.OpenEventStream(ctx)
	if err != nil {
		t.Fatalf("OpenEventStream: %v", err)
	}
	defer stream.Close()

	var got []DownlinkEvent
	timeout := time.After(2 * time.Second)
	for len(got) < 3 {
		select {
		case ev, ok := <-stream.Events():
			if !ok {
				t.Fatalf("stream closed early after %d events", len(got))
			}
			got = append(got, ev)
		case <-timeout:
			t.Fatalf("timed out; got %d events: %+v", len(got), got)
		}
	}

	// First frame is the heartbeat comment surfaced as a ping.
	if got[0].Event != DownlinkEventPing {
		t.Errorf("event[0] = %q, want ping", got[0].Event)
	}
	if got[1].Event != DownlinkEventSync {
		t.Errorf("event[1] = %q, want sync", got[1].Event)
	}
	if got[2].Event != DownlinkEventCommand {
		t.Errorf("event[2] = %q, want command", got[2].Event)
	}
	if !strings.Contains(string(got[2].Data), "cancel") {
		t.Errorf("command data missing payload: %s", got[2].Data)
	}
}

func TestOpenEventStream_MultiLineData(t *testing.T) {
	// Two data: lines for one event must join with a newline.
	body := "event: sync\ndata: line1\ndata: line2\n\n"
	srv := sseServer(t, body)
	defer srv.Close()

	_, client := newTestSyncClient(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := client.OpenEventStream(ctx)
	if err != nil {
		t.Fatalf("OpenEventStream: %v", err)
	}
	defer stream.Close()

	select {
	case ev := <-stream.Events():
		if string(ev.Data) != "line1\nline2" {
			t.Errorf("data = %q, want \"line1\\nline2\"", ev.Data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestOpenEventStream_RejectsOversizedEvent(t *testing.T) {
	// Many data: continuation lines until past eventsSSEMaxEventBytes → the
	// reader surfaces an error and closes the channel (so the loop reconnects).
	var b strings.Builder
	b.WriteString("event: command\n")
	chunk := "data: " + strings.Repeat("x", 4096) + "\n"
	for b.Len() < eventsSSEMaxEventBytes+8192 {
		b.WriteString(chunk)
	}
	b.WriteString("\n")
	srv := sseServer(t, b.String())
	defer srv.Close()

	_, client := newTestSyncClient(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := client.OpenEventStream(ctx)
	if err != nil {
		t.Fatalf("OpenEventStream: %v", err)
	}
	defer stream.Close()

	// Drain until the channel closes (the oversized event must NOT be emitted).
	timeout := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-stream.Events():
			if !ok {
				if stream.Err() == nil {
					t.Error("expected an error after oversized event, got nil")
				}
				return
			}
			if ev.Event == DownlinkEventCommand {
				t.Fatalf("oversized command event must not be dispatched")
			}
		case <-timeout:
			t.Fatal("timed out; channel never closed after oversized event")
		}
	}
}

func TestOpenEventStream_Non200ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"not found"}`)
	}))
	defer srv.Close()

	_, client := newTestSyncClient(srv.URL)
	_, err := client.OpenEventStream(context.Background())
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusNotFound {
		t.Errorf("expected HTTPError 404, got %v", err)
	}
}

func TestEventStream_CloseCancelsRead(t *testing.T) {
	srv := sseServer(t, ": connected\n\n")
	defer srv.Close()

	_, client := newTestSyncClient(srv.URL)
	stream, err := client.OpenEventStream(context.Background())
	if err != nil {
		t.Fatalf("OpenEventStream: %v", err)
	}

	// Drain the initial ping.
	select {
	case <-stream.Events():
	case <-time.After(2 * time.Second):
		t.Fatal("no initial ping")
	}

	done := make(chan struct{})
	go func() {
		stream.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close() did not return — read goroutine leaked")
	}
}

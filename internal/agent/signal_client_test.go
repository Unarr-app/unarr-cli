package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSSEServer streams a fixed set of SSE events then closes the connection.
func fakeSSEServer(t *testing.T, msgs []SignalMessage, holdOpenAfter bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("server: ResponseWriter is not a Flusher")
		}
		fmt.Fprint(w, "retry: 1500\n\n")
		flusher.Flush()
		for _, m := range msgs {
			data, _ := json.Marshal(m)
			fmt.Fprintf(w, "id: %d\nevent: signal\ndata: %s\n\n", m.TS, data)
			flusher.Flush()
		}
		// Send a heartbeat comment to verify it's ignored.
		fmt.Fprint(w, ": heartbeat\n\n")
		flusher.Flush()
		if holdOpenAfter {
			// Hold the connection until the client disconnects so the test can
			// exercise stream.Close().
			<-r.Context().Done()
		}
	}))
}

func TestSignalStreamReadsMessages(t *testing.T) {
	want := []SignalMessage{
		{From: SignalRoleBrowser, Type: SignalMsgOffer, Payload: "{sdp:1}", TS: 1},
		{From: SignalRoleBrowser, Type: SignalMsgCandidate, Payload: "{cand:1}", TS: 2},
	}
	srv := fakeSSEServer(t, want, false)
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "test-ua")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	stream, err := c.OpenSignalStream(ctx, "session-1")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer stream.Close()

	var got []SignalMessage
	for m := range stream.Events() {
		got = append(got, m)
		if len(got) == len(want) {
			break
		}
	}
	if len(got) != len(want) {
		t.Fatalf("got %d messages, want %d", len(got), len(want))
	}
	for i, m := range got {
		if m.From != want[i].From || m.Type != want[i].Type || m.Payload != want[i].Payload {
			t.Errorf("[%d] mismatch: %+v want %+v", i, m, want[i])
		}
	}
}

func TestSignalStreamPropagatesAuthError(t *testing.T) {
	srv := fakeSSEServer(t, nil, false)
	defer srv.Close()

	c := NewClient(srv.URL, "wrong-key", "test-ua")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := c.OpenSignalStream(ctx, "session-1")
	if err == nil {
		t.Fatal("expected auth error, got nil")
	}
}

func TestSignalStreamCloseCancelsRead(t *testing.T) {
	srv := fakeSSEServer(t, nil, true)
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "test-ua")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := c.OpenSignalStream(ctx, "session-1")
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// Close on a separate goroutine then make sure the events channel drains.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(50 * time.Millisecond)
		stream.Close()
	}()

	for range stream.Events() {
		// drain
	}
	wg.Wait()
}

// TestSignalStreamRejectsOversizedEvent verifies that a hostile or buggy
// server sending an unbounded `data:` event surfaces an error and stops
// the reader instead of growing daemon memory forever.
func TestSignalStreamRejectsOversizedEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		// Send many data: continuation lines until we blow past the
		// per-event cap. Each chunk is a short legitimate-looking line.
		chunk := "data: " + strings.Repeat("x", 4096) + "\n"
		fmt.Fprint(w, "event: signal\n")
		for i := 0; i < (sseMaxEventBytes/4096)+8; i++ {
			fmt.Fprint(w, chunk)
		}
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "test-ua")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	stream, err := c.OpenSignalStream(ctx, "session-overflow")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer stream.Close()

	for range stream.Events() {
		// Should never receive a parsed event — the over-sized buffer must
		// be rejected before dispatch.
	}
	if err := stream.Err(); err == nil {
		t.Fatal("expected error from oversized event, got nil")
	}
}

func TestPostSignalSendsCorrectBody(t *testing.T) {
	var bodySeen map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&bodySeen)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "test-ua")
	err := c.PostSignal(context.Background(), "sess-x", SignalMessage{
		Type:    SignalMsgAnswer,
		Payload: "{sdp:answer}",
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if bodySeen["from"] != string(SignalRoleAgent) {
		t.Errorf("expected from=agent, got %v", bodySeen["from"])
	}
	if bodySeen["type"] != string(SignalMsgAnswer) {
		t.Errorf("expected type=answer, got %v", bodySeen["type"])
	}
	if bodySeen["payload"] != "{sdp:answer}" {
		t.Errorf("expected payload mismatch, got %v", bodySeen["payload"])
	}
}

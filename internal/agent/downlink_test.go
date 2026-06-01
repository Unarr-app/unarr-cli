package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDownlinkMode(t *testing.T) {
	cases := map[string]string{
		"":        "auto",
		"auto":    "auto",
		"AUTO":    "auto",
		"  sse  ": "sse",
		"sse":     "sse",
		"poll":    "poll",
		"garbage": "auto",
	}
	for in, want := range cases {
		sc, _ := newTestSyncClient("http://127.0.0.1:0")
		sc.cfg.Downlink = in
		if got := sc.downlinkMode(); got != want {
			t.Errorf("downlinkMode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHandleDownlinkEvent_SyncNudge(t *testing.T) {
	sc, _ := newTestSyncClient("http://127.0.0.1:0")
	sc.handleDownlinkEvent(DownlinkEvent{Event: DownlinkEventSync, Data: json.RawMessage(`{"reason":"wake"}`)})

	select {
	case <-sc.SyncNow:
		// good — TriggerSync fired
	default:
		t.Error("sync event did not trigger an immediate sync")
	}
}

func TestHandleDownlinkEvent_TypedControls(t *testing.T) {
	sc, _ := newTestSyncClient("http://127.0.0.1:0")

	var gotAction, gotTask string
	var gotDelete bool
	sc.OnControl = func(action, taskID string, deleteFiles bool) {
		gotAction, gotTask, gotDelete = action, taskID, deleteFiles
	}

	payload := `{"controls":[{"action":"cancel","taskId":"task-xyz","deleteFiles":true}]}`
	sc.handleDownlinkEvent(DownlinkEvent{Event: DownlinkEventCommand, Data: json.RawMessage(payload)})

	if gotAction != "cancel" || gotTask != "task-xyz" || !gotDelete {
		t.Errorf("OnControl got (%q,%q,%v), want (cancel,task-xyz,true)", gotAction, gotTask, gotDelete)
	}
}

func TestHandleDownlinkEvent_PingIsLivenessOnly(t *testing.T) {
	sc, _ := newTestSyncClient("http://127.0.0.1:0")
	controlCalled := false
	sc.OnControl = func(string, string, bool) { controlCalled = true }

	sc.handleDownlinkEvent(DownlinkEvent{Event: DownlinkEventPing})

	if controlCalled {
		t.Error("ping must not invoke OnControl")
	}
	select {
	case <-sc.SyncNow:
		t.Error("ping must not trigger a sync")
	default:
	}
}

func TestHandleDownlinkEvent_BadPayloadNoPanic(t *testing.T) {
	sc, _ := newTestSyncClient("http://127.0.0.1:0")
	sc.OnControl = func(string, string, bool) { t.Error("OnControl must not fire on bad payload") }
	// Should log + return, not panic.
	sc.handleDownlinkEvent(DownlinkEvent{Event: DownlinkEventCommand, Data: json.RawMessage(`{not json`)})
}

// TestRunEventStreamOnce_Healthy: a server that sends a heartbeat then a sync
// event, then closes → runEventStreamOnce returns true (healthy) and the sync
// nudge fired.
func TestRunEventStreamOnce_Healthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, _ := w.(http.Flusher)
		w.Write([]byte(": hb\n\n"))
		if f != nil {
			f.Flush()
		}
		w.Write([]byte("event: sync\ndata: {}\n\n"))
		if f != nil {
			f.Flush()
		}
		// Return → response body closes → stream ends.
	}))
	defer srv.Close()

	sc, _ := newTestSyncClient(srv.URL)
	sc.livenessTimeout = 500 * time.Millisecond

	healthy := sc.runEventStreamOnce(context.Background())
	if !healthy {
		t.Error("expected healthy=true after receiving frames")
	}
	select {
	case <-sc.SyncNow:
	default:
		t.Error("expected a sync nudge from the sync event")
	}
}

// TestRunEventStreamOnce_DeadOrBuffered: server connects 200 OK but sends
// nothing → liveness deadline fires → returns false (so auto mode falls back).
func TestRunEventStreamOnce_DeadOrBuffered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Send NO frames — simulate a silently-buffering proxy.
		<-r.Context().Done()
	}))
	defer srv.Close()

	sc, _ := newTestSyncClient(srv.URL)
	sc.livenessTimeout = 150 * time.Millisecond

	start := time.Now()
	healthy := sc.runEventStreamOnce(context.Background())
	if healthy {
		t.Error("expected healthy=false when no frame arrives within liveness deadline")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("liveness deadline did not fire promptly (took %s)", elapsed)
	}
}

// TestRunEventStreamOnce_PreambleThenStall: a partial-buffering proxy that
// flushes the connect preamble (one heartbeat) then goes silent must be treated
// as UNHEALTHY (false), so the auto fallback eventually triggers. This is the
// common buffering mode the zero-frame test doesn't cover.
func TestRunEventStreamOnce_PreambleThenStall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, _ := w.(http.Flusher)
		// Flush ONE heartbeat (the preamble) then stall — never send more.
		w.Write([]byte(": connected hb=15000\n\n"))
		if f != nil {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	sc, _ := newTestSyncClient(srv.URL)
	sc.livenessTimeout = 150 * time.Millisecond

	if sc.runEventStreamOnce(context.Background()) {
		t.Error("a stream that flushes one ping then stalls must be unhealthy (else fallback never triggers)")
	}
}

// TestRunEventStreamOnce_ConnectFail: dead server → false, no hang.
func TestRunEventStreamOnce_ConnectFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // port now refuses

	sc, _ := newTestSyncClient(url)
	sc.livenessTimeout = 500 * time.Millisecond

	if sc.runEventStreamOnce(context.Background()) {
		t.Error("expected healthy=false on connect failure")
	}
}

// TestRunEventStreamOnce_CtxCancel: cancelling ctx returns promptly.
func TestRunEventStreamOnce_CtxCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	sc, _ := newTestSyncClient(srv.URL)
	sc.livenessTimeout = 10 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	done := make(chan struct{})
	go func() {
		sc.runEventStreamOnce(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runEventStreamOnce did not return after ctx cancel")
	}
}

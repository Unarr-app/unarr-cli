package agent

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMirrorRoundTripper_FailoverOn503(t *testing.T) {
	var primaryHits, mirrorHits int
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryHits++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer primary.Close()
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mirrorHits++
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	}))
	defer mirror.Close()

	pool := NewMirrorPool(primary.URL, []string{mirror.URL})
	rt := NewMirrorRoundTripper(pool, http.DefaultTransport)
	req, _ := http.NewRequest(http.MethodGet, primary.URL+"/api/v1/search", nil)

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if primaryHits != 1 || mirrorHits != 1 {
		t.Errorf("hits primary=%d mirror=%d, want 1/1", primaryHits, mirrorHits)
	}
}

func TestMirrorRoundTripper_NoFailoverOn404(t *testing.T) {
	var mirrorHits int
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer primary.Close()
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mirrorHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer mirror.Close()

	pool := NewMirrorPool(primary.URL, []string{mirror.URL})
	rt := NewMirrorRoundTripper(pool, http.DefaultTransport)
	req, _ := http.NewRequest(http.MethodGet, primary.URL+"/x", nil)

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (surfaced, not retried)", resp.StatusCode)
	}
	if mirrorHits != 0 {
		t.Errorf("mirror hit %d times — must NOT fail over on 404", mirrorHits)
	}
}

func TestMirrorRoundTripper_FailoverOnConnRefused(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // port now refuses connections

	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mirror.Close()

	pool := NewMirrorPool(deadURL, []string{mirror.URL})
	rt := NewMirrorRoundTripper(pool, http.DefaultTransport)
	req, _ := http.NewRequest(http.MethodGet, deadURL+"/x", nil)

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip should have failed over, got: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 after failover", resp.StatusCode)
	}
}

func TestMirrorRoundTripper_ReplaysBodyOnFailover(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer primary.Close()
	var gotBody string
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer mirror.Close()

	pool := NewMirrorPool(primary.URL, []string{mirror.URL})
	rt := NewMirrorRoundTripper(pool, http.DefaultTransport)
	req, _ := http.NewRequest(http.MethodPost, primary.URL+"/x", strings.NewReader("payload"))

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()
	if gotBody != "payload" {
		t.Errorf("mirror received body %q, want \"payload\" (body must be replayed on failover)", gotBody)
	}
}

func TestMirrorRoundTripper_NonReplayableBodyNoFailover(t *testing.T) {
	var primaryHits, mirrorHits int
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryHits++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer primary.Close()
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mirrorHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer mirror.Close()

	pool := NewMirrorPool(primary.URL, []string{mirror.URL})
	rt := NewMirrorRoundTripper(pool, http.DefaultTransport)
	// A body with no GetBody can't be replayed → must be sent once, no failover.
	req, _ := http.NewRequest(http.MethodPost, primary.URL+"/x", io.NopCloser(strings.NewReader("payload")))
	req.GetBody = nil

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (single attempt, no failover)", resp.StatusCode)
	}
	if primaryHits != 1 || mirrorHits != 0 {
		t.Errorf("hits primary=%d mirror=%d, want 1/0 (non-replayable body must not fail over)", primaryHits, mirrorHits)
	}
}

func TestMirrorRoundTripper_SingleMirrorSurfaces503(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer primary.Close()

	pool := NewMirrorPool(primary.URL, nil)
	rt := NewMirrorRoundTripper(pool, http.DefaultTransport)
	req, _ := http.NewRequest(http.MethodGet, primary.URL+"/x", nil)

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 surfaced (no mirror to fail over to)", resp.StatusCode)
	}
}

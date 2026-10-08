package remotefs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Real existing-API baseline: headers finish, the body stays open and silent.
func TestBoundaryHTTPBodyStallBaseline(t *testing.T) {
	started := make(chan struct{})
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-9/10")
		w.Header().Set("Content-Length", "10")
		w.WriteHeader(http.StatusPartialContent)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer cdn.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := NewHTTPReader(ctx, HTTPClient(), &Link{Resolve: func(context.Context) (string, error) { return cdn.URL, nil }}, 10)
	r.bodyIdleTimeout = 50 * time.Millisecond
	defer r.Close()
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(r); done <- err }()
	<-started
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("silent body succeeded")
		}
	case <-time.After(250 * time.Millisecond):
		t.Error("existing API has no body progress bound within probe window")
	}
	cancel()
}

func TestBoundaryHTTPStalledGETSlots(t *testing.T) {
	started := make(chan struct{}, 32)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthy" {
			http.ServeContent(w, r, "healthy.bin", time.Time{}, bytes.NewReader([]byte("exact")))
			return
		}
		w.Header().Set("Content-Range", "bytes 0-9/10")
		w.Header().Set("Content-Length", "10")
		w.WriteHeader(http.StatusPartialContent)
		w.(http.Flusher).Flush()
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer cdn.Close()
	fs := New()
	client := HTTPClient()
	l := &Link{Resolve: func(context.Context) (string, error) { return cdn.URL, nil }}
	if err := fs.Replace([]Entry{{Path: "v.bin", Key: "v", Size: 10, Open: func(ctx context.Context) (io.ReadSeekCloser, error) {
		reader := NewHTTPReader(ctx, client, l, 10)
		reader.bodyIdleTimeout = 150 * time.Millisecond
		return reader, nil
	}}}); err != nil {
		t.Fatal(err)
	}
	dav := httptest.NewServer(Handler(fs, "u", "p"))
	defer dav.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 32)
	for range 32 {
		go func() {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, dav.URL+"/dav/v.bin", nil)
			req.SetBasicAuth("u", "p")
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				_, err = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
			done <- err
		}()
	}
	for range 32 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("GET did not reach CDN")
		}
	}
	for range 32 {
		select {
		case <-done:
		case <-time.After(250 * time.Millisecond):
			t.Error("stalled GET still holds slot")
			cancel()
			return
		}
	}
	request, _ := http.NewRequest(http.MethodGet, dav.URL+"/dav/v.bin", nil)
	request.SetBasicAuth("u", "p")
	// A later healthy path must be able to acquire a GET slot.
	healthy := &Link{Resolve: func(context.Context) (string, error) { return cdn.URL + "/healthy", nil }}
	if err := fs.Replace([]Entry{{Path: "healthy.bin", Key: "h", Size: 5, Open: func(ctx context.Context) (io.ReadSeekCloser, error) {
		return NewHTTPReader(ctx, client, healthy, 5), nil
	}}}); err != nil {
		t.Fatal(err)
	}
	request.URL.Path = "/dav/healthy.bin"
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || err != nil || string(got) != "exact" {
		t.Fatalf("healthy GET after expiry: %s, %q, %v", resp.Status, got, err)
	}
}

func TestBoundaryHTTPHealthyProgressBeyondIdle(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "12")
		w.Header().Set("Content-Range", "bytes 0-11/12")
		w.WriteHeader(http.StatusPartialContent)
		for range 12 {
			_, _ = w.Write([]byte("x"))
			w.(http.Flusher).Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer cdn.Close()
	r := NewHTTPReader(context.Background(), HTTPClient(), &Link{Resolve: func(context.Context) (string, error) { return cdn.URL, nil }}, 12)
	r.bodyIdleTimeout = 100 * time.Millisecond
	defer r.Close()
	start := time.Now()
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "xxxxxxxxxxxx" || time.Since(start) < r.bodyIdleTimeout {
		t.Fatalf("healthy long stream: %q %v after %v", got, err, time.Since(start))
	}
}

type blockedWriter struct {
	deadline time.Time
	header   http.Header
}

func (w *blockedWriter) Header() http.Header                { return w.header }
func (*blockedWriter) WriteHeader(int)                      {}
func (w *blockedWriter) SetWriteDeadline(d time.Time) error { w.deadline = d; return nil }
func (w *blockedWriter) Write([]byte) (int, error) {
	if w.deadline.IsZero() {
		return 0, fmt.Errorf("missing downstream write bound")
	}
	<-time.After(time.Until(w.deadline))
	return 0, context.DeadlineExceeded
}

func TestBoundaryDownstreamWriteBound(t *testing.T) {
	w := &blockedWriter{header: make(http.Header)}
	start := time.Now()
	_, err := (progressWriter{ResponseWriter: w, timeout: 30 * time.Millisecond}).Write([]byte("x"))
	if err != context.DeadlineExceeded || time.Since(start) > 250*time.Millisecond {
		t.Fatalf("downstream write: %v after %v", err, time.Since(start))
	}
}

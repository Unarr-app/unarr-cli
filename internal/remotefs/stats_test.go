package remotefs

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestReadStatsCountBytesAndActiveReads(t *testing.T) {
	_, before := ReadStats()

	pw := progressWriter{ResponseWriter: httptest.NewRecorder(), timeout: time.Second}
	if _, err := pw.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	if _, after := ReadStats(); after-before != 10 {
		t.Fatalf("bytes delta = %d, want 10", after-before)
	}

	// A GET holds an "active read" for exactly as long as it runs.
	inside := make(chan int64, 1)
	dav := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		a, _ := ReadStats()
		inside <- a
	})
	base, _ := ReadStats()
	serveDAVRead(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/dav/x", nil), dav, make(chan struct{}, 1))
	if got := <-inside; got != base+1 {
		t.Fatalf("active during read = %d, want %d", got, base+1)
	}
	if a, _ := ReadStats(); a != base {
		t.Fatalf("active after read = %d, want %d (leaked)", a, base)
	}
}

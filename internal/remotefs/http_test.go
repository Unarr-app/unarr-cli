package remotefs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPReaderRangesReuseAndExpiry(t *testing.T) {
	data := bytes.Repeat([]byte("abcdefgh"), 1000)
	var calls, resolve atomic.Int32
	var rangesMu sync.Mutex
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		rangesMu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		rangesMu.Unlock()
		if r.URL.Path == "/expired" {
			http.Error(w, "expired", http.StatusForbidden)
			return
		}
		http.ServeContent(w, r, "file.bin", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()
	link := &Link{url: srv.URL + "/expired", Resolve: func(context.Context) (string, error) { resolve.Add(1); return srv.URL + "/live", nil }}
	ctx := withRange(context.Background(), "bytes=4000-4999")
	r := NewHTTPReader(ctx, srv.Client(), link, int64(len(data)))
	defer r.Close()
	_, _ = r.Seek(4000, io.SeekStart)
	buf := make([]byte, 1000)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, data[4000:5000]) {
		t.Fatal("bad bytes")
	}
	if calls.Load() != 2 || resolve.Load() != 1 {
		t.Fatal(calls.Load(), resolve.Load())
	}
	for _, rng := range ranges {
		if rng != "bytes=4000-4999" {
			t.Fatal(rng)
		}
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	all, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(all, data) {
		t.Fatal(len(all), err)
	}
}

func TestHTTPReaderRejectsIncorrectResponses(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		code                   int
		cr, cl, encoding, body string
	}{
		{"offset", 206, "bytes 0-4/10", "5", "", "xxxxx"},
		{"total", 206, "bytes 5-9/11", "5", "", "xxxxx"},
		{"length", 206, "bytes 5-9/10", "4", "", "xxxx"},
		{"ignored", 200, "", "10", "", "0123456789"},
		{"encoding", 206, "bytes 5-9/10", "5", "gzip", "xxxxx"},
		{"416BeforeEOF", 416, "bytes */10", "0", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Range", tc.cr)
				w.Header().Set("Content-Length", tc.cl)
				w.Header().Set("Content-Encoding", tc.encoding)
				w.WriteHeader(tc.code)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			r := NewHTTPReader(context.Background(), srv.Client(), &Link{url: srv.URL}, 10)
			defer r.Close()
			_, _ = r.Seek(5, io.SeekStart)
			n, err := r.Read(make([]byte, 5))
			if n != 0 || err == nil {
				t.Fatalf("served invalid response: n=%d err=%v", n, err)
			}
		})
	}
}

func TestHTTPReaderTruncationAndCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-9/10")
		w.Header().Set("Content-Length", "10")
		w.WriteHeader(206)
		_, _ = io.WriteString(w, "short")
	}))
	defer srv.Close()
	r := NewHTTPReader(context.Background(), srv.Client(), &Link{url: srv.URL}, 10)
	defer r.Close()
	_, err := io.ReadAll(r)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r2 := NewHTTPReader(ctx, srv.Client(), &Link{url: srv.URL}, 10)
	defer r2.Close()
	if _, err := r2.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestLinkCoalescesConcurrentRenewal(t *testing.T) {
	var count atomic.Int32
	start := make(chan struct{})
	release := make(chan struct{})
	l := &Link{url: "http://old", Resolve: func(context.Context) (string, error) {
		if count.Add(1) == 1 {
			close(start)
		}
		<-release
		return "http://new", nil
	}}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u, err := l.get(context.Background(), "http://old")
			if u != "http://new" || err != nil {
				t.Errorf("%s %v", u, err)
			}
		}()
	}
	<-start
	close(release)
	wg.Wait()
	if count.Load() != 1 {
		t.Fatal(count.Load())
	}
}

func TestDAVDoesNotSniffBeforeRemoteTail(t *testing.T) {
	var rangeGot string
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeGot = r.Header.Get("Range")
		w.Header().Set("Content-Range", "bytes 900-999/1000")
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(206)
		_, _ = io.WriteString(w, strings.Repeat("x", 100))
	}))
	defer cdn.Close()
	f := New()
	e := memoryEntry("rd/file.unknown", make([]byte, 1000))
	link := &Link{url: cdn.URL}
	e.Open = func(ctx context.Context) (io.ReadSeekCloser, error) {
		return NewHTTPReader(ctx, cdn.Client(), link, 1000), nil
	}
	mustReplace(t, f, []Entry{e})
	r := httptest.NewRequest("GET", "http://localhost/dav/rd/file.unknown", nil)
	r.SetBasicAuth("u", "p")
	r.Header.Set("Range", "bytes=900-999")
	w := httptest.NewRecorder()
	Handler(f, "u", "p").ServeHTTP(w, r)
	if w.Code != 206 || w.Body.Len() != 100 || rangeGot != "bytes=900-999" {
		t.Fatal(w.Code, w.Body.Len(), rangeGot)
	}
}

func BenchmarkHTTPRange(b *testing.B) {
	data := bytes.Repeat([]byte("abcdefgh"), 1<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "file.bin", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()
	for _, size := range []int{4096, 1 << 20, len(data)} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			buf := make([]byte, 32<<10)
			link := &Link{url: srv.URL}
			ctx := withRange(context.Background(), fmt.Sprintf("bytes=0-%d", size-1))
			b.SetBytes(int64(size))
			b.ReportAllocs()
			for b.Loop() {
				r := NewHTTPReader(ctx, srv.Client(), link, int64(len(data)))
				n, err := io.CopyBuffer(io.Discard, io.LimitReader(r, int64(size)), buf)
				_ = r.Close()
				if err != nil || n != int64(size) {
					b.Fatal(n, err)
				}
			}
		})
	}
}

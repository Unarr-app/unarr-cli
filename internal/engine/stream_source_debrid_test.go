package engine

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// rangeServer serves a fixed byte slice with full HTTP Range support via
// http.ServeContent (the same machinery a real debrid CDN exposes). Records
// the number of GETs so a test can assert that a seek triggers exactly one
// reopen rather than a full re-download.
func rangeServer(data []byte) (*httptest.Server, *int) {
	gets := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets++
		}
		http.ServeContent(w, r, "movie.mp4", time.Time{}, bytes.NewReader(data))
	}))
	return srv, &gets
}

func makeData(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251) // non-trivial, deterministic pattern
	}
	return b
}

func TestDebridProviderHeadSize(t *testing.T) {
	data := makeData(4096)
	srv, _ := rangeServer(data)
	defer srv.Close()

	// HEAD reports the real size; fallback is ignored when HEAD succeeds.
	p, err := NewDebridFileProvider(context.Background(), srv.URL, "movie.mp4", 999)
	if err != nil {
		t.Fatalf("NewDebridFileProvider: %v", err)
	}
	if got := p.FileSize(); got != int64(len(data)) {
		t.Fatalf("FileSize from HEAD = %d, want %d", got, len(data))
	}
	if p.FileName() != "movie.mp4" {
		t.Fatalf("FileName = %q, want movie.mp4", p.FileName())
	}
}

func TestDebridProviderNameFromURLWhenNoExtension(t *testing.T) {
	// The web may pass a torrent title with no extension (its file-name
	// fallback). The provider must derive the name from the URL so the served
	// Content-Type is video/mp4, not application/octet-stream.
	data := makeData(1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "x", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()
	p, err := NewDebridFileProvider(context.Background(), srv.URL+"/Movie.2026.1080p.mp4?token=abc", "Project Hail Mary (2026) 1080p web", 0)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if got := p.FileName(); got != "Movie.2026.1080p.mp4" {
		t.Fatalf("FileName = %q, want Movie.2026.1080p.mp4 (derived from URL)", got)
	}
	// A passed name WITH an extension is kept as-is.
	p2, _ := NewDebridFileProvider(context.Background(), srv.URL+"/whatever.mp4", "Nice Title.mp4", 0)
	if got := p2.FileName(); got != "Nice Title.mp4" {
		t.Fatalf("FileName = %q, want Nice Title.mp4 (kept)", got)
	}
}

func TestDebridProviderFallbackSizeWhenNoHead(t *testing.T) {
	// Server that refuses HEAD (405) but serves GET — provider must fall back
	// to the size the web reported.
	data := makeData(2048)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		http.ServeContent(w, r, "movie.mp4", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()

	p, err := NewDebridFileProvider(context.Background(), srv.URL, "movie.mp4", int64(len(data)))
	if err != nil {
		t.Fatalf("NewDebridFileProvider: %v", err)
	}
	if got := p.FileSize(); got != int64(len(data)) {
		t.Fatalf("FileSize fallback = %d, want %d", got, len(data))
	}
}

func TestDebridProviderNoSizeFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed) // no HEAD, no usable GET
	}))
	defer srv.Close()
	// No HEAD size and fallback 0 → must error (ServeContent can't range-serve
	// size 0 without handing the browser an empty file).
	if _, err := NewDebridFileProvider(context.Background(), srv.URL, "", 0); err == nil {
		t.Fatal("expected error when size is unknown, got nil")
	}
	if _, err := NewDebridFileProvider(context.Background(), "", "movie.mp4", 100); err == nil {
		t.Fatal("expected error for empty URL, got nil")
	}
}

func TestDebridReaderSequentialRead(t *testing.T) {
	data := makeData(100_000)
	srv, gets := rangeServer(data)
	defer srv.Close()

	p, err := NewDebridFileProvider(context.Background(), srv.URL, "movie.mp4", 0)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	rd := p.NewFileReader(context.Background())
	defer rd.Close()

	got, err := io.ReadAll(rd)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("sequential read mismatch: got %d bytes, want %d", len(got), len(data))
	}
	// A pure sequential read holds a single body to EOF → exactly one GET.
	if *gets != 1 {
		t.Fatalf("sequential read issued %d GETs, want 1", *gets)
	}
}

func TestDebridReaderSeekEndReportsSize(t *testing.T) {
	data := makeData(5000)
	srv, _ := rangeServer(data)
	defer srv.Close()
	p, _ := NewDebridFileProvider(context.Background(), srv.URL, "movie.mp4", 0)
	rd := p.NewFileReader(context.Background())
	defer rd.Close()

	// http.ServeContent calls Seek(0, SeekEnd) to learn the size — must be
	// network-free and return the total.
	size, err := rd.Seek(0, io.SeekEnd)
	if err != nil {
		t.Fatalf("Seek end: %v", err)
	}
	if size != int64(len(data)) {
		t.Fatalf("SeekEnd = %d, want %d", size, len(data))
	}
}

func TestDebridReaderSeekThenRead(t *testing.T) {
	data := makeData(50_000)
	srv, gets := rangeServer(data)
	defer srv.Close()
	p, _ := NewDebridFileProvider(context.Background(), srv.URL, "movie.mp4", 0)
	rd := p.NewFileReader(context.Background())
	defer rd.Close()

	const off = 12_345
	if _, err := rd.Seek(off, io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	got, err := io.ReadAll(rd)
	if err != nil {
		t.Fatalf("ReadAll after seek: %v", err)
	}
	if !bytes.Equal(got, data[off:]) {
		t.Fatalf("tail mismatch: got %d bytes, want %d", len(got), len(data)-off)
	}
	// Seek is network-free; the read after it is the only GET.
	if *gets != 1 {
		t.Fatalf("seek+read issued %d GETs, want 1", *gets)
	}
}

func TestDebridReaderServeContentRoundTrip(t *testing.T) {
	// Drive the reader exactly like StreamServer does: hand it to
	// http.ServeContent and issue a ranged request. Verifies the reader is a
	// correct io.ReadSeeker for the production serving path.
	data := makeData(80_000)
	srv, _ := rangeServer(data)
	defer srv.Close()
	p, _ := NewDebridFileProvider(context.Background(), srv.URL, "movie.mp4", 0)

	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rd := p.NewFileReader(r.Context())
		defer rd.Close()
		http.ServeContent(w, r, p.FileName(), time.Time{}, rd)
	}))
	defer front.Close()

	req, _ := http.NewRequest(http.MethodGet, front.URL, nil)
	req.Header.Set("Range", "bytes=10000-19999")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("ranged GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	want := data[10000:20000]
	if !bytes.Equal(body, want) {
		t.Fatalf("ranged body mismatch: got %d bytes", len(body))
	}
}

func TestDebridReaderSeekPastEnd(t *testing.T) {
	data := makeData(1000)
	srv, _ := rangeServer(data)
	defer srv.Close()
	p, _ := NewDebridFileProvider(context.Background(), srv.URL, "movie.mp4", 0)
	rd := p.NewFileReader(context.Background())
	defer rd.Close()

	// Seeking at/past size then reading yields EOF, no error, no bytes.
	if _, err := rd.Seek(int64(len(data)), io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	n, err := rd.Read(make([]byte, 16))
	if n != 0 || err != io.EOF {
		t.Fatalf("read past end = (%d, %v), want (0, EOF)", n, err)
	}
}

func TestDebridReaderRejectsServerIgnoringRange(t *testing.T) {
	// A server that always returns 200 (ignores Range) is only safe at pos 0.
	// A reopen at a non-zero offset (after a seek) must error rather than serve
	// misaligned bytes.
	data := makeData(4000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "4000")
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	}))
	defer srv.Close()
	p, err := NewDebridFileProvider(context.Background(), srv.URL, "movie.mp4", int64(len(data)))
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	rd := p.NewFileReader(context.Background())
	defer rd.Close()

	if _, err := rd.Seek(1000, io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	if _, err := rd.Read(make([]byte, 16)); err == nil {
		t.Fatal("expected error when server ignores Range at non-zero offset, got nil")
	}
}

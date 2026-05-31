package engine

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeGrowing is a GrowingSource backed by a fixed byte slice. When final is
// true it behaves like a completed remux (ReadAt returns io.EOF at the end);
// est overrides the advertised estimate (0 = use len(data)).
type fakeGrowing struct {
	data  []byte
	final bool
	est   int64
}

func (f *fakeGrowing) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[off:])
	if int(off)+n >= len(f.data) {
		return n, io.EOF
	}
	return n, nil
}
func (f *fakeGrowing) Size() int64 { return int64(len(f.data)) }
func (f *fakeGrowing) Final() bool { return f.final }
func (f *fakeGrowing) EstimatedSize() int64 {
	if f.est > 0 {
		return f.est
	}
	return int64(len(f.data))
}
func (f *fakeGrowing) FileName() string { return "movie.mp4" }
func (f *fakeGrowing) Close() error     { return nil }

func TestParseByteRange(t *testing.T) {
	cases := []struct {
		in         string
		start, end int64
	}{
		{"", 0, -1},
		{"bytes=0-", 0, -1},
		{"bytes=100-", 100, -1},
		{"bytes=5-9", 5, 9},
		{"bytes=0-0", 0, 0},
		{"bytes=10-19,40-49", 10, 19}, // first range only
		{"bytes=-500", 0, -1},         // suffix unsupported → open from 0
		{"garbage", 0, -1},
		{"bytes=", 0, -1},
	}
	for _, c := range cases {
		s, e := parseByteRange(c.in)
		if s != c.start || e != c.end {
			t.Errorf("parseByteRange(%q) = (%d,%d), want (%d,%d)", c.in, s, e, c.start, c.end)
		}
	}
}

func TestServeGrowing_FinalFullRequest(t *testing.T) {
	data := []byte("0123456789abcdef")
	src := &fakeGrowing{data: data, final: true}
	ss := &StreamServer{}

	req := httptest.NewRequest(http.MethodGet, "/stream", nil)
	rec := httptest.NewRecorder()
	ss.serveGrowing(rec, req, src)

	res := rec.Result()
	if res.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", res.StatusCode)
	}
	if got := res.Header.Get("Content-Range"); got != "bytes 0-15/16" {
		t.Errorf("Content-Range = %q, want bytes 0-15/16", got)
	}
	if got := res.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}
	if got := res.Header.Get("Content-Type"); got != "video/mp4" {
		t.Errorf("Content-Type = %q, want video/mp4", got)
	}
	// Final + open-ended → exact Content-Length.
	if got := res.Header.Get("Content-Length"); got != "16" {
		t.Errorf("Content-Length = %q, want 16", got)
	}
	if body := rec.Body.String(); body != string(data) {
		t.Errorf("body = %q, want %q", body, string(data))
	}
}

func TestServeGrowing_OffsetRange(t *testing.T) {
	data := []byte("0123456789abcdef")
	src := &fakeGrowing{data: data, final: true}
	ss := &StreamServer{}

	req := httptest.NewRequest(http.MethodGet, "/stream", nil)
	req.Header.Set("Range", "bytes=10-")
	rec := httptest.NewRecorder()
	ss.serveGrowing(rec, req, src)

	res := rec.Result()
	if res.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", res.StatusCode)
	}
	if got := res.Header.Get("Content-Range"); got != "bytes 10-15/16" {
		t.Errorf("Content-Range = %q, want bytes 10-15/16", got)
	}
	if body := rec.Body.String(); body != "abcdef" {
		t.Errorf("body = %q, want abcdef", body)
	}
}

func TestServeGrowing_BoundedRange(t *testing.T) {
	data := []byte("0123456789abcdef")
	src := &fakeGrowing{data: data, final: true}
	ss := &StreamServer{}

	req := httptest.NewRequest(http.MethodGet, "/stream", nil)
	req.Header.Set("Range", "bytes=5-9")
	rec := httptest.NewRecorder()
	ss.serveGrowing(rec, req, src)

	res := rec.Result()
	if res.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", res.StatusCode)
	}
	if got := res.Header.Get("Content-Range"); got != "bytes 5-9/16" {
		t.Errorf("Content-Range = %q, want bytes 5-9/16", got)
	}
	if body := rec.Body.String(); body != "56789" {
		t.Errorf("body = %q, want 56789 (exactly the requested 5 bytes)", body)
	}
}

func TestServeGrowing_EstimateUsedWhileNotFinal(t *testing.T) {
	// Not final: only 8 bytes produced, but estimate says 100. The advertised
	// total is the estimate (scrubber timeline); body is what exists so far.
	src := &fakeGrowing{data: []byte("01234567"), final: false, est: 100}
	ss := &StreamServer{}

	req := httptest.NewRequest(http.MethodGet, "/stream", nil)
	rec := httptest.NewRecorder()
	ss.serveGrowing(rec, req, src)

	res := rec.Result()
	if res.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", res.StatusCode)
	}
	if got := res.Header.Get("Content-Range"); got != "bytes 0-99/100" {
		t.Errorf("Content-Range = %q, want bytes 0-99/100 (estimate)", got)
	}
	// Not final → no exact Content-Length (chunked) so we never promise bytes
	// a still-running remux might not produce.
	if got := res.Header.Get("Content-Length"); got != "" {
		t.Errorf("Content-Length = %q, want empty (chunked) while not final", got)
	}
	if body := rec.Body.String(); body != "01234567" {
		t.Errorf("body = %q, want 01234567 (bytes produced so far)", body)
	}
}

func TestServeGrowing_HeadProbe(t *testing.T) {
	src := &fakeGrowing{data: make([]byte, 0), final: false, est: 4242}
	ss := &StreamServer{}

	req := httptest.NewRequest(http.MethodHead, "/stream", nil)
	rec := httptest.NewRecorder()
	ss.serveGrowing(rec, req, src)

	res := rec.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", res.StatusCode)
	}
	if got := res.Header.Get("Content-Length"); got != "4242" {
		t.Errorf("HEAD Content-Length = %q, want 4242", got)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD body = %d bytes, want 0", rec.Body.Len())
	}
}

func TestServeGrowing_RangeBeyondTotal(t *testing.T) {
	src := &fakeGrowing{data: []byte("0123456789"), final: true}
	ss := &StreamServer{}

	req := httptest.NewRequest(http.MethodGet, "/stream", nil)
	req.Header.Set("Range", "bytes=999-")
	rec := httptest.NewRecorder()
	ss.serveGrowing(rec, req, src)

	if rec.Result().StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("status = %d, want 416", rec.Result().StatusCode)
	}
}

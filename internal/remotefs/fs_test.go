package remotefs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memoryReader struct{ *bytes.Reader }

func (memoryReader) Close() error { return nil }
func memoryEntry(p string, data []byte) Entry {
	return Entry{Path: p, Key: p, Size: int64(len(data)), Modified: time.Unix(1700000000, 0).UTC(), Open: func(context.Context) (io.ReadSeekCloser, error) { return memoryReader{bytes.NewReader(data)}, nil }}
}
func mustReplace(t testing.TB, f *FS, entries []Entry) {
	t.Helper()
	if err := f.Replace(entries); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataNeverOpensSource(t *testing.T) {
	f := New()
	var opens atomic.Int32
	e := memoryEntry("debrid/movie/file.unknown", []byte("0123456789"))
	e.Open = func(context.Context) (io.ReadSeekCloser, error) { opens.Add(1); return nil, errors.New("offline") }
	mustReplace(t, f, []Entry{e})
	h := Handler(f, "u", "p")
	for _, method := range []string{"HEAD", "PROPFIND"} {
		r := httptest.NewRequest(method, "http://localhost/dav/debrid/movie/file.unknown", nil)
		r.SetBasicAuth("u", "p")
		r.Header.Set("Depth", "0")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := 200
		if method == "PROPFIND" {
			want = 207
		}
		if w.Code != want {
			t.Fatalf("%s: %d %s", method, w.Code, w.Body)
		}
	}
	if opens.Load() != 0 {
		t.Fatalf("metadata opened source %d times", opens.Load())
	}
}

func TestSnapshotHandlesAndWriteProtection(t *testing.T) {
	f := New()
	ctx := context.Background()
	mustReplace(t, f, []Entry{memoryEntry("a/z", []byte("old")), memoryEntry("a/b", []byte("b"))})
	d, err := f.OpenFile(ctx, "/a", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	file, err := f.OpenFile(ctx, "/a/z", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	mustReplace(t, f, []Entry{memoryEntry("a/new", []byte("new"))})
	items, err := d.Readdir(1)
	if err != nil || len(items) != 1 || items[0].Name() != "b" {
		t.Fatalf("old listing: %v %v", items, err)
	}
	items, err = d.Readdir(1)
	if err != nil || items[0].Name() != "z" {
		t.Fatal(items, err)
	}
	if _, err = d.Readdir(1); err != io.EOF {
		t.Fatal(err)
	}
	b, err := io.ReadAll(file)
	if err != nil || string(b) != "old" {
		t.Fatal(string(b), err)
	}
	for _, flags := range []int{os.O_WRONLY, os.O_RDWR, os.O_TRUNC, os.O_APPEND, os.O_CREATE, os.O_EXCL} {
		if _, err := f.OpenFile(ctx, "a/new", flags, 0); !errors.Is(err, os.ErrPermission) {
			t.Fatalf("flags %d: %v", flags, err)
		}
	}
	for _, p := range []string{"../a/new", "a/../a/new", "a//new", "a/./new", "a/new/no"} {
		if _, err := f.Stat(ctx, p); err == nil {
			t.Fatalf("accepted %q", p)
		}
	}
	for _, entries := range [][]Entry{
		{memoryEntry("../bad", nil)},
		{memoryEntry("a", nil), memoryEntry("a/b", nil)},
		{memoryEntry("a/b", nil), memoryEntry("a", nil)},
		{memoryEntry("a", nil), memoryEntry("a", nil)},
	} {
		if err := f.Replace(entries); err == nil {
			t.Fatal("accepted invalid tree")
		}
	}
	if _, err := f.Stat(ctx, "a/new"); err != nil {
		t.Fatal("invalid replace lost old snapshot", err)
	}
}

func TestDAVRangeAndReadOnly(t *testing.T) {
	f := New()
	data := []byte(strings.Repeat("abcdef", 100))
	mustReplace(t, f, []Entry{memoryEntry("rd/a & b/video.mkv", data)})
	h := Handler(f, "u", "p")
	for _, tc := range []struct {
		method, rng string
		status      int
		body        string
	}{
		{"GET", "bytes=17-22", 206, string(data[17:23])},
		{"GET", "bytes=-8", 206, string(data[len(data)-8:])},
		{"GET", "bytes=600-", 416, ""},
		{"PUT", "", 405, ""}, {"DELETE", "", 405, ""}, {"MOVE", "", 405, ""}, {"COPY", "", 405, ""}, {"MKCOL", "", 405, ""}, {"LOCK", "", 405, ""},
	} {
		r := httptest.NewRequest(tc.method, "http://localhost/dav/rd/a%20%26%20b/video.mkv", nil)
		r.SetBasicAuth("u", "p")
		r.Header.Set("Range", tc.rng)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s %s: %d %s", tc.method, tc.rng, w.Code, w.Body)
		}
		if tc.body != "" && w.Body.String() != tc.body {
			t.Fatalf("wrong bytes %q", w.Body)
		}
	}
	r := httptest.NewRequest("PROPFIND", "http://localhost/dav/", nil)
	r.SetBasicAuth("u", "p")
	r.Header.Set("Depth", "infinity")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	r = httptest.NewRequest("HEAD", "http://localhost/dav/rd/a%20%26%20b/video.mkv", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
}

func TestConcurrentRefreshAndRead(t *testing.T) {
	f := New()
	a := []Entry{memoryEntry("rd/movie", []byte("abcdef"))}
	mustReplace(t, f, a)
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				file, err := f.OpenFile(context.Background(), "rd/movie", 0, 0)
				if err != nil {
					t.Error(err)
					return
				}
				b, err := io.ReadAll(file)
				_ = file.Close()
				if err != nil || string(b) != "abcdef" {
					t.Errorf("%q %v", b, err)
				}
			}
		}()
	}
	for i := 0; i < 100; i++ {
		mustReplace(t, f, a)
	}
	wg.Wait()
}

func TestSeeksBoundsCancellationAndClosed(t *testing.T) {
	f := New()
	mustReplace(t, f, []Entry{memoryEntry("movie", []byte("abcdef"))})
	ctx, cancel := context.WithCancel(context.Background())
	r, _ := f.OpenFile(ctx, "movie", 0, 0)
	if _, err := r.Seek(-1, io.SeekStart); err == nil {
		t.Fatal("negative seek accepted")
	}
	if _, err := r.Seek(1<<63-1, io.SeekEnd); err == nil {
		t.Fatal("overflow accepted")
	}
	if _, err := r.Seek(8, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(make([]byte, 1)); err != io.EOF {
		t.Fatal(err)
	}
	_, _ = r.Seek(0, io.SeekStart)
	cancel()
	if _, err := r.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_ = r.Close()
	if _, err := r.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal(err)
	}
}

func BenchmarkMetadata(b *testing.B) {
	for _, size := range []int{1000, 100000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			f := New()
			entries := make([]Entry, size)
			for i := range entries {
				entries[i] = memoryEntry(fmt.Sprintf("rd/release-%d/movie.mkv", i), nil)
			}
			mustReplace(b, f, entries)
			ctx := context.Background()
			b.Run("StatParallel", func(b *testing.B) {
				b.ReportAllocs()
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						if _, err := f.Stat(ctx, "rd/release-123/movie.mkv"); err != nil {
							b.Fatal(err)
						}
					}
				})
			})
			b.Run("StatSerial", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if _, err := f.Stat(ctx, "rd/release-123/movie.mkv"); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("StatRandomParallel", func(b *testing.B) {
				var worker atomic.Int64
				b.ReportAllocs()
				b.RunParallel(func(pb *testing.PB) {
					i := int(worker.Add(1)) * 9973 % size
					for pb.Next() {
						i = (i + 7919) % size
						if _, err := f.Stat(ctx, entries[i].Path); err != nil {
							b.Fatal(err)
						}
					}
				})
			})
			b.Run("OpenStatClose", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					r, err := f.OpenFile(ctx, "rd/release-123/movie.mkv", 0, 0)
					if err != nil {
						b.Fatal(err)
					}
					_, _ = r.Stat()
					_ = r.Close()
				}
			})
			b.Run("PROPFINDRelease", func(b *testing.B) {
				h := Handler(f, "u", "p")
				r := httptest.NewRequest("PROPFIND", "http://localhost/dav/rd/release-123", nil)
				r.SetBasicAuth("u", "p")
				r.Header.Set("Depth", "1")
				b.ReportAllocs()
				for b.Loop() {
					w := httptest.NewRecorder()
					h.ServeHTTP(w, r)
					if w.Code != http.StatusMultiStatus {
						b.Fatal(w.Code)
					}
				}
			})
		})
	}
}

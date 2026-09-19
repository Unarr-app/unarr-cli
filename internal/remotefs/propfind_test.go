package remotefs

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/net/webdav"
)

func TestPropfindCacheInvalidationAndPropertyIsolation(t *testing.T) {
	f := New()
	mustReplace(t, f, []Entry{memoryEntry("dir/old", []byte("old"))})
	cache := newPropCache()
	h := handlerWithCache(f, "u", "p", cache)
	request := func(body string) string {
		r := httptest.NewRequest("PROPFIND", "http://localhost/dav/dir", strings.NewReader(body))
		r.SetBasicAuth("u", "p")
		r.Header.Set("Depth", "1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 207 {
			t.Fatal(w.Code, w.Body)
		}
		return w.Body.String()
	}
	first := request("")
	if second := request(""); first != second {
		t.Fatal("cache changed XML")
	}
	if len(cache.entries) != 1 {
		t.Fatal("response not cached")
	}
	sizeOnly := request(`<D:propfind xmlns:D="DAV:"><D:prop><D:getcontentlength/></D:prop></D:propfind>`)
	if sizeOnly == first || strings.Contains(sizeOnly, "getetag") {
		t.Fatal("property variants mixed")
	}
	mustReplace(t, f, []Entry{memoryEntry("dir/new", []byte("new"))})
	updated := request("")
	if strings.Contains(updated, "/old") || !strings.Contains(updated, "/new") {
		t.Fatal("stale cached directory", updated)
	}
}

func TestPropfindCacheBoundedAndLargeResponseStreams(t *testing.T) {
	c := newPropCache()
	for i := 0; i < 1000; i++ {
		c.put(&propResponse{key: propKey{path: fmt.Sprint(i)}, data: make([]byte, 64<<10)})
	}
	if c.bytes > propCacheBytes || len(c.entries) > propCacheEntries {
		t.Fatal("unbounded cache", c.bytes, len(c.entries))
	}
	f := New()
	entries := make([]Entry, 4000)
	for i := range entries {
		entries[i] = memoryEntry(fmt.Sprintf("dir/file-%d", i), nil)
	}
	mustReplace(t, f, entries)
	dav := &webdav.Handler{Prefix: "/dav", FileSystem: f, LockSystem: webdav.NewMemLS()}
	r := httptest.NewRequest("PROPFIND", "http://localhost/dav/dir", nil)
	r.Header.Set("Depth", "1")
	w := httptest.NewRecorder()
	c = newPropCache()
	c.serve(w, r, dav, f.current.Load())
	if w.Code != http.StatusMultiStatus || w.Body.Len() < propCacheBodyLimit {
		t.Fatal(w.Code, w.Body.Len())
	}
	if len(c.entries) != 0 {
		t.Fatal("oversized response cached")
	}
}

func BenchmarkPropfindCache(b *testing.B) {
	f := New()
	mustReplace(b, f, []Entry{memoryEntry("dir/video.mkv", nil)})
	for _, cached := range []bool{false, true} {
		b.Run(fmt.Sprintf("cached=%v", cached), func(b *testing.B) {
			var c *propCache
			if cached {
				c = newPropCache()
			}
			h := handlerWithCache(f, "u", "p", c)
			r := httptest.NewRequest("PROPFIND", "http://localhost/dav/dir", nil)
			r.SetBasicAuth("u", "p")
			r.Header.Set("Depth", "1")
			h.ServeHTTP(httptest.NewRecorder(), r)
			b.ReportAllocs()
			for b.Loop() {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 207 {
					b.Fatal(w.Code)
				}
			}
		})
	}
}

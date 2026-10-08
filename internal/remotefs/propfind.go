package remotefs

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"io"
	"net/http"
	"sync"

	"golang.org/x/net/webdav"
)

// Immutable snapshot generations make XML listings cacheable without TTLs or
// stale metadata. Limits bound both bytes and entry count. Keys never retain a
// snapshot pointer, so a cached response cannot keep an obsolete tree alive.
const propCacheBytes = 16 << 20
const propCacheEntries = 256
const propCacheBodyLimit = 1 << 20

type propKey struct {
	generation  uint64
	path, depth string
	body        [32]byte
}
type propResponse struct {
	key         propKey
	contentType string
	data        []byte
}
type propCache struct {
	mu      sync.Mutex
	entries map[propKey]*list.Element
	lru     *list.List
	bytes   int
}

func newPropCache() *propCache {
	return &propCache{entries: make(map[propKey]*list.Element), lru: list.New()}
}
func (c *propCache) get(key propKey) *propResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el := c.entries[key]; el != nil {
		c.lru.MoveToFront(el)
		return el.Value.(*propResponse)
	}
	return nil
}
func (c *propCache) put(response *propResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[response.key]; exists {
		return
	}
	for c.bytes+cap(response.data) > propCacheBytes || len(c.entries) >= propCacheEntries {
		el := c.lru.Back()
		old := el.Value.(*propResponse)
		c.bytes -= cap(old.data)
		delete(c.entries, old.key)
		c.lru.Remove(el)
	}
	c.bytes += cap(response.data)
	c.entries[response.key] = c.lru.PushFront(response)
}

func (c *propCache) serve(w http.ResponseWriter, r *http.Request, dav *webdav.Handler, snapshot *snapshot) {
	// Only small property requests are cached; unusual large XML requests still
	// go through the standard parser and the endpoint's 1 MiB request limit.
	body, err := io.ReadAll(io.LimitReader(r.Body, 4097))
	if err != nil {
		http.Error(w, "invalid property request", http.StatusBadRequest)
		return
	}
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(body), r.Body), r.Body}
	fs := &FS{}
	fs.current.Store(snapshot)
	view := *dav
	view.FileSystem = fs
	if len(body) > 4096 {
		view.ServeHTTP(w, r)
		return
	}
	key := propKey{snapshot.generation, r.URL.EscapedPath(), r.Header.Get("Depth"), sha256.Sum256(body)}
	if hit := c.get(key); hit != nil {
		w.Header().Set("Content-Type", hit.contentType)
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write(hit.data)
		return
	}
	// Stream the first response normally. Large directories are never buffered
	// in full, and failures/partial writes are never installed in the cache.
	capture := &propCapture{ResponseWriter: w}
	view.ServeHTTP(capture, r)
	if capture.status == http.StatusMultiStatus && !capture.overflow && !capture.failed {
		c.put(&propResponse{key: key, contentType: w.Header().Get("Content-Type"), data: capture.body.Bytes()})
	}
}

type propCapture struct {
	http.ResponseWriter
	body             bytes.Buffer
	status           int
	overflow, failed bool
}

func (w *propCapture) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *propCapture) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	if err != nil || n != len(p) {
		w.failed = true
	}
	if !w.overflow {
		if w.body.Len()+len(p) > propCacheBodyLimit {
			w.overflow = true
			w.body = bytes.Buffer{}
		} else {
			_, _ = w.body.Write(p)
		}
	}
	return n, err
}

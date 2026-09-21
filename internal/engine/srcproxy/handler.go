package srcproxy

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// handle serves GET/HEAD for the fg/ and bg/ aliases of the source.
func (p *Proxy) handle(w http.ResponseWriter, r *http.Request) {
	background, ok := p.route(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	first, last, ranged, ok := parseRange(r.Header.Get("Range"))
	if !ok {
		http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if !background {
		p.foreground.Add(1)
		defer p.foreground.Add(-1)
	}
	rd := &reader{p: p, ctx: r.Context()}
	defer rd.close()

	// The total size is needed for Content-Range; learn it from the upstream
	// answer to this very request so no extra round trip is spent on it.
	if p.size.Load() < 0 {
		if err := rd.seek(first / blockSize * blockSize); err != nil {
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
			return
		}
	}
	size := p.size.Load()
	if first >= size {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if last < 0 || last >= size {
		last = size - 1
	}
	writeRangeHeaders(w, first, last, size, ranged)
	if r.Method == http.MethodHead {
		return
	}
	p.stream(w, rd, span{first, last}, background)
}

func (p *Proxy) route(r *http.Request) (background, ok bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false, false
	}
	rest, found := strings.CutPrefix(r.URL.Path, p.prefix)
	if !found {
		return false, false
	}
	switch rest {
	case "fg/" + p.name:
		return false, true
	case "bg/" + p.name:
		return true, true
	}
	return false, false
}

func writeRangeHeaders(w http.ResponseWriter, first, last, size int64, ranged bool) {
	h := w.Header()
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Length", strconv.FormatInt(last-first+1, 10))
	if !ranged {
		w.WriteHeader(http.StatusOK)
		return
	}
	h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, last, size))
	w.WriteHeader(http.StatusPartialContent)
}

type span struct{ first, last int64 }

// stream copies [first,last] to the client block by block: cache first, then
// the request's sequential upstream reader. It stops silently when the client
// goes away — ffmpeg closes as soon as it has the packets it wanted.
func (p *Proxy) stream(w http.ResponseWriter, rd *reader, sp span, background bool) {
	buf := make([]byte, blockSize)
	for pos := sp.first; pos <= sp.last; {
		idx := pos / blockSize
		n, hit := p.store.get(idx, buf)
		if !hit {
			var err error
			if n, err = p.fill(rd, idx, buf, background); err != nil {
				return
			}
		}
		lo := pos - idx*blockSize
		hi := min(int64(n), sp.last-idx*blockSize+1)
		if lo >= hi {
			return
		}
		if _, err := w.Write(buf[lo:hi]); err != nil {
			return
		}
		p.stats.servedBytes.Add(hi - lo)
		if hit {
			p.stats.cacheHitBytes.Add(hi - lo)
		}
		pos += hi - lo
	}
}

// fill fetches a missing block. Background readers first yield to foreground
// traffic, and only populate the pinned region: a bulk sequential pass through
// the LRU would evict exactly the blocks playback is about to reuse.
func (p *Proxy) fill(rd *reader, idx int64, buf []byte, background bool) (int, error) {
	if background {
		if err := p.yield(rd.ctx); err != nil {
			return 0, err
		}
	}
	n, err := rd.fetch(idx, buf)
	if err != nil {
		return 0, err
	}
	if !background || p.pinned(idx) {
		p.store.put(idx, buf[:n])
	}
	return n, nil
}

// yield blocks a background reader while any foreground request is in flight,
// for at most backgroundDrip.
func (p *Proxy) yield(ctx context.Context) error {
	deadline := time.Now().Add(backgroundDrip)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for p.foreground.Load() > 0 && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
	return ctx.Err()
}

// parseRange understands the single-range forms ffmpeg and the seek-index
// reader send: "bytes=a-" and "bytes=a-b". last=-1 means open-ended.
func parseRange(h string) (first, last int64, ranged, ok bool) {
	if h == "" {
		return 0, -1, false, true
	}
	spec, found := strings.CutPrefix(h, "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, 0, false, false
	}
	a, b, found := strings.Cut(spec, "-")
	if !found || a == "" {
		return 0, 0, false, false // suffix ranges ("-n") are never sent by our readers
	}
	first, err := strconv.ParseInt(a, 10, 64)
	if err != nil || first < 0 {
		return 0, 0, false, false
	}
	if b == "" {
		return first, -1, true, true
	}
	last, err = strconv.ParseInt(b, 10, 64)
	if err != nil || last < first {
		return 0, 0, false, false
	}
	return first, last, true, true
}

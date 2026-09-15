package stream

import (
	"os"
	"sync"
	"sync/atomic"

	"github.com/Unarr-app/unarr-cli/internal/usenet/yenc"
)

// Decoded article buffers live outside the Go heap and are recycled. The collector
// lets the heap grow to about twice what survived its last cycle before running
// again, so article data on the heap cost roughly double its size in resident
// memory: a 128 MiB cache showed as ~370 MiB. Mapped outside the heap, the data
// counts once, is invisible to the collector's pacing, and goes back to the OS
// the moment its buffer is unmapped — without tuning the collector for the rest
// of the process. Where that is unavailable (see bufalloc_*.go) buffers stay on
// the heap and are only recycled.
//
// Memory outside the heap is freed by hand, so a buffer may only be reused or
// unmapped once nothing can still read it. The cache counts the holders of every
// part decoded into a pooled buffer (ArticleCache.refs): its own entry, each
// reader that took the part from the cache or from a flight, the fetch that
// produced it, and a reader's current article. Parts that were not decoded into
// such a buffer are never counted and belong to the collector.

const (
	// maxPooledBufferBytes caps the memory idle buffers may hold,
	// maxPooledBuffers how many there may be (get scans them).
	maxPooledBufferBytes = 32 << 20
	maxPooledBuffers     = 64
	// offHeapMinBytes is the smallest buffer worth a mapping of its own. Smaller
	// ones stay on the heap, and a buffer's capacity tells which it is.
	offHeapMinBytes = 64 << 10
)

// poisonReleasedBuffers overwrites every buffer returned to the pool, so a reader
// still using one serves bytes a byte-comparing test notices. Tests only.
var poisonReleasedBuffers atomic.Bool

// bufferPool holds idle article buffers for later fetches. Every buffer it is
// given must have come from get.
type bufferPool struct {
	mu     sync.Mutex
	free   [][]byte // oldest first
	bytes  int64
	reused atomic.Int64 // gets served from the pool (diagnostics and tests)
}

var (
	articleBuffers bufferPool
	pageSize       = os.Getpagesize()
)

// get returns an empty buffer with room for need bytes. It reuses the smallest
// idle buffer whose capacity is at most an eighth above need (or a page, which a
// mapping may round up by), and allocates otherwise: need is an encoded size, a
// few percent above the decoded article, and decodeArticle trims a part whose
// buffer is a quarter above its data.
func (p *bufferPool) get(need int) []byte {
	if b := p.take(need); b != nil {
		p.reused.Add(1)
		return b
	}
	if need >= offHeapMinBytes {
		if b := allocOffHeap(need); b != nil {
			return b
		}
	}
	// The mapping failed, or need is small. Kept below offHeapMinBytes, where heap
	// buffers belong: one over it would be taken for a mapping when freed. A larger
	// article grows past it on the heap and is never pooled.
	return make([]byte, 0, min(need, offHeapMinBytes-1))
}

// take removes and returns the best idle buffer for need, or nil.
func (p *bufferPool) take(need int) []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	best, limit := -1, need+max(need/8, pageSize)
	for i, b := range p.free {
		if c := cap(b); c >= need && c <= limit && (best < 0 || c < cap(p.free[best])) {
			best = i
		}
	}
	if best < 0 {
		return nil
	}
	b := p.free[best]
	last := len(p.free) - 1
	copy(p.free[best:], p.free[best+1:]) // keeps the rest oldest first
	p.free[last] = nil
	p.free = p.free[:last]
	p.bytes -= int64(cap(b))
	return b[:0]
}

// put makes buf available to a later get. A full pool drops its oldest buffers:
// they have gone longest without fitting a request, and keeping them instead
// would let buffers of a size nobody asks for any more (a finished stream's, a
// file's short last article) crowd out the ones in use. Nobody may use buf
// afterwards.
func (p *bufferPool) put(buf []byte) {
	if cap(buf) == 0 {
		return
	}
	if poisonReleasedBuffers.Load() {
		full := buf[:cap(buf)]
		for i := range full {
			full[i] = 0xDB
		}
	}
	if cap(buf) > maxPooledBufferBytes {
		releaseBuffers([][]byte{buf})
		return
	}
	p.mu.Lock()
	drop := 0
	for drop < len(p.free) && (len(p.free)-drop >= maxPooledBuffers || p.bytes+int64(cap(buf)) > maxPooledBufferBytes) {
		p.bytes -= int64(cap(p.free[drop]))
		drop++
	}
	dropped := append([][]byte(nil), p.free[:drop]...)
	p.free = append(p.free[drop:], buf[:0])
	p.bytes += int64(cap(buf))
	p.mu.Unlock()
	releaseBuffers(dropped)
}

// drain frees every idle buffer, so the memory can go back to the OS.
func (p *bufferPool) drain() {
	p.mu.Lock()
	dropped := p.free
	p.free, p.bytes = nil, 0
	p.mu.Unlock()
	releaseBuffers(dropped)
}

// releaseBuffers unmaps the off-heap buffers among bufs; heap ones are left to
// the collector.
func releaseBuffers(bufs [][]byte) {
	for _, b := range bufs {
		if cap(b) >= offHeapMinBytes {
			freeOffHeap(b)
		}
	}
}

// sameBacking reports whether a and b share their first backing byte.
func sameBacking(a, b []byte) bool {
	return cap(a) > 0 && cap(b) > 0 && &a[:1][0] == &b[:1][0]
}

// trackPart registers part, decoded into a buffer no one else references, as
// recyclable with one holder: the fetch that produced it.
func (c *ArticleCache) trackPart(part *yenc.Part) {
	c.mu.Lock()
	c.refs[part] = 1
	c.mu.Unlock()
}

// retainLocked adds n holders to part if it is tracked.
func (c *ArticleCache) retainLocked(part *yenc.Part, n int) {
	if _, ok := c.refs[part]; ok && n > 0 {
		c.refs[part] += n
	}
}

// releasePart drops one holder of part (nil or untracked: no-op), recycling its
// buffer once none is left.
func (c *ArticleCache) releasePart(part *yenc.Part) {
	if part == nil {
		return
	}
	c.mu.Lock()
	buf := c.releaseLocked(part)
	c.mu.Unlock()
	articleBuffers.put(buf)
}

// releaseLocked drops one holder of part and returns its buffer when that was
// the last holder. The part's Data is cleared so that a use nobody counted fails
// loudly instead of reading a recycled buffer.
func (c *ArticleCache) releaseLocked(part *yenc.Part) []byte {
	n, ok := c.refs[part]
	if !ok {
		return nil
	}
	if n > 1 {
		c.refs[part] = n - 1
		return nil
	}
	delete(c.refs, part)
	buf := part.Data
	part.Data = nil
	return buf
}

// abandon settles a waiter that stopped waiting for fl: before fl finishes it is
// no longer counted; after, it holds a reference to fl's part that it drops.
func (s *CacheScope) abandon(fl *flight) {
	c := s.c
	c.mu.Lock()
	var buf []byte
	if !fl.finished {
		fl.waiters--
	} else if fl.part != nil {
		buf = c.releaseLocked(fl.part)
	}
	c.mu.Unlock()
	articleBuffers.put(buf)
}

package stream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nzb"
	"github.com/Unarr-app/unarr-cli/internal/usenet/yenc"
)

// Tuning defaults. All are overridable by white-box tests (unexported fields on
// Reader) so the retry/backoff path can be exercised without real latency.
const (
	defaultCacheCap     = 16 // decoded articles kept resident (~750 KB each)
	defaultReadaheadK   = 4  // articles prefetched ahead of the read cursor
	defaultMaxAttempts  = 3  // per-article fetch attempts before giving up
	defaultRetryBackoff = 500 * time.Millisecond
)

// ArticleFetcher fetches a raw (yEnc) article body by message-id. *nntp.Client
// satisfies it; injecting the interface keeps the Reader testable against the
// in-memory fake server with no real network.
type ArticleFetcher interface {
	Body(ctx context.Context, messageID string) ([]byte, error)
}

// Reader is an io.ReadSeekCloser over ONE logical file assembled from the NNTP
// articles of an nzb.File. It mirrors debridRangeReader: Seek is network-free
// (it only moves the logical position), and Read lazily fetches+decodes the
// article covering that position and serves the requested slice. http.ServeContent
// drives it exactly like a local file — Seek(0, SeekEnd) for size, Seek to the
// range start, then sequential Reads — so a user seek in the player becomes a
// single article fetch, never a full re-download.
//
// Correctness comes from the OffsetIndex: article boundaries start as a cheap
// Segment.Bytes estimate and become byte-EXACT via yenc =ypart headers Observed
// as articles are fetched. Read probes forward/back from the estimated segment
// until the fetched part's exact range actually covers the position, so an
// off-by-a-little estimate never yields wrong bytes.
//
// A Reader is single-consumer: Read/Seek/Close must be called from one goroutine
// (as http.ServeContent does). Only the internal read-ahead goroutines run
// concurrently, and they touch just the mutex-guarded cache — never the index.
type Reader struct {
	ctx     context.Context
	cancel  context.CancelFunc
	fetcher ArticleFetcher
	ix      *OffsetIndex

	pos int64 // logical read position (moved by Seek, advanced by Read)

	mu       sync.Mutex     // guards cache + inflight
	cache    *articleCache  // decoded articles by segment index
	inflight map[int]bool   // segments a read-ahead goroutine is currently fetching
	wg       sync.WaitGroup // tracks read-ahead goroutines (drained by Close)

	readaheadK   int
	maxAttempts  int
	retryBackoff time.Duration
}

// NewReader builds a Reader over f's articles. ix must be the index for f (built
// with NewOffsetIndex(f)); if nil, one is constructed so callers that only have
// the file still get a working reader. The returned Reader owns a child context
// cancelled by Close, which stops any in-flight read-ahead.
func NewReader(ctx context.Context, fetcher ArticleFetcher, f nzb.File, ix *OffsetIndex) *Reader {
	if ix == nil {
		ix = NewOffsetIndex(f)
	}
	cctx, cancel := context.WithCancel(ctx)
	return &Reader{
		ctx:          cctx,
		cancel:       cancel,
		fetcher:      fetcher,
		ix:           ix,
		cache:        newArticleCache(defaultCacheCap),
		inflight:     make(map[int]bool),
		readaheadK:   defaultReadaheadK,
		maxAttempts:  defaultMaxAttempts,
		retryBackoff: defaultRetryBackoff,
	}
}

// Read serves bytes from the current position, fetching (and decoding) exactly
// the article that covers it. It returns io.EOF once the position reaches the
// exact file size. A read-ahead of the following articles is kicked off after a
// successful read so sequential playback stays ahead of the cursor.
func (r *Reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.ix.SizeExact() && r.pos >= r.ix.FileSize() {
		return 0, io.EOF
	}
	part, segIdx, err := r.articleForOffset(r.pos)
	if err != nil {
		return 0, err
	}
	dataStart := part.Begin - 1 // yEnc begin is 1-based inclusive
	off := r.pos - dataStart
	if off < 0 || off >= int64(len(part.Data)) {
		return 0, fmt.Errorf("usenet reader: offset %d outside decoded article [%d,%d)",
			r.pos, dataStart, dataStart+int64(len(part.Data)))
	}
	n := copy(p, part.Data[off:])
	r.pos += int64(n)
	r.triggerReadahead(segIdx)
	return n, nil
}

// Seek moves the logical position. SeekEnd needs the exact size, so it fetches a
// single article first if none has been observed yet (cheap with random access).
func (r *Reader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.pos + offset
	case io.SeekEnd:
		if err := r.ensureSizeExact(); err != nil {
			return 0, err
		}
		abs = r.ix.FileSize() + offset
	default:
		return 0, fmt.Errorf("usenet reader: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, errors.New("usenet reader: negative position")
	}
	r.pos = abs
	return abs, nil
}

// Close cancels any in-flight read-ahead and waits for those goroutines to exit,
// so no fetch outlives the reader. It never closes the underlying fetcher (the
// NNTP client pool is shared and owned elsewhere).
func (r *Reader) Close() error {
	r.cancel()
	r.wg.Wait()
	return nil
}

// --- internal: locating the covering article ---

// articleForOffset returns the decoded part whose EXACT byte range covers pos,
// plus its segment index. It starts from the index's (possibly estimated) guess
// and probes forward/back one segment at a time, Observing each fetched part to
// sharpen the map, until pos lands inside a fetched article. The probe is bounded
// by the segment count, so it always terminates; io.EOF is returned when pos is
// past the last article's real data.
func (r *Reader) articleForOffset(pos int64) (*yenc.Part, int, error) {
	n := r.ix.SegmentCount()
	if n == 0 {
		return nil, 0, io.EOF
	}
	segIdx, _, _, ok := r.ix.Locate(pos)
	if !ok {
		return nil, 0, io.EOF
	}
	for probe := 0; probe <= n; probe++ {
		part, err := r.fetchArticle(segIdx)
		if err != nil {
			return nil, segIdx, err
		}
		r.ix.Observe(segIdx, part)
		switch classifyOffset(pos, part) {
		case -1:
			if segIdx == 0 {
				return nil, segIdx, fmt.Errorf("usenet reader: offset %d precedes first article", pos)
			}
			segIdx--
		case 1:
			if segIdx == n-1 {
				return nil, segIdx, io.EOF
			}
			segIdx++
		default:
			return part, segIdx, nil
		}
	}
	return nil, segIdx, fmt.Errorf("usenet reader: could not locate article for offset %d in %d segments", pos, n)
}

// classifyOffset reports where pos sits relative to a decoded part's exact range:
// -1 before it, +1 at/after its end, 0 inside. yEnc Begin is 1-based inclusive so
// the 0-based data range is [Begin-1, End).
func classifyOffset(pos int64, part *yenc.Part) int {
	start := part.Begin - 1
	end := part.End // 0-based exclusive end == 1-based inclusive End
	switch {
	case pos < start:
		return -1
	case pos >= end:
		return 1
	default:
		return 0
	}
}

// ensureSizeExact fetches and observes the first article when the file size is
// still an estimate, making FileSize byte-exact (and, for a uniform posting,
// pinning the whole offset map). No-op once any article has been observed.
func (r *Reader) ensureSizeExact() error {
	if r.ix.SizeExact() || r.ix.SegmentCount() == 0 {
		return nil
	}
	part, err := r.fetchArticle(0)
	if err != nil {
		return fmt.Errorf("usenet reader: establish size: %w", err)
	}
	r.ix.Observe(0, part)
	return nil
}

// --- internal: fetch, cache, retry, read-ahead ---

// fetchArticle returns the decoded part for segment segIdx, serving it from the
// cache when present and otherwise fetching (with retry) and caching it.
func (r *Reader) fetchArticle(segIdx int) (*yenc.Part, error) {
	r.mu.Lock()
	if part, ok := r.cache.get(segIdx); ok {
		r.mu.Unlock()
		return part, nil
	}
	r.mu.Unlock()

	part, err := r.fetchDecodeRetry(r.ix.Segment(segIdx).MessageID)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.cache.put(segIdx, part)
	r.mu.Unlock()
	return part, nil
}

// fetchDecodeRetry fetches and yEnc-decodes one article, retrying transient
// failures (a not-yet-propagated article, a dropped connection, a corrupt body)
// up to maxAttempts with a bounded backoff. Every failure is logged; the final
// error is wrapped so the caller can degrade cleanly rather than hang.
func (r *Reader) fetchDecodeRetry(messageID string) (*yenc.Part, error) {
	var lastErr error
	for attempt := 0; attempt < r.maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(r.retryBackoff):
			case <-r.ctx.Done():
				return nil, r.ctx.Err()
			}
		}
		raw, err := r.fetcher.Body(r.ctx, messageID)
		if err == nil {
			var part *yenc.Part
			if part, err = yenc.DecodeBytes(raw); err == nil {
				return part, nil
			}
			err = fmt.Errorf("decode: %w", err)
		}
		lastErr = err
		log.Printf("[usenet-stream] article %s attempt %d/%d failed: %v",
			messageID, attempt+1, r.maxAttempts, err)
	}
	return nil, fmt.Errorf("usenet reader: article %s failed after %d attempts: %w",
		messageID, r.maxAttempts, lastErr)
}

// triggerReadahead prefetches up to readaheadK articles following fromSeg so the
// next sequential reads hit the cache. Message-ids are resolved here on the read
// goroutine; the spawned goroutines never touch the index.
func (r *Reader) triggerReadahead(fromSeg int) {
	if r.readaheadK <= 0 {
		return
	}
	n := r.ix.SegmentCount()
	for j := fromSeg + 1; j <= fromSeg+r.readaheadK && j < n; j++ {
		r.prefetch(j, r.ix.Segment(j).MessageID)
	}
}

// prefetch fetches segment segIdx in the background unless it is already cached
// or already being fetched. Dedup + cache writes happen under r.mu.
func (r *Reader) prefetch(segIdx int, messageID string) {
	r.mu.Lock()
	if r.cache.has(segIdx) || r.inflight[segIdx] {
		r.mu.Unlock()
		return
	}
	r.inflight[segIdx] = true
	r.mu.Unlock()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		part, err := r.fetchDecodeRetry(messageID)
		r.mu.Lock()
		delete(r.inflight, segIdx)
		if err == nil {
			r.cache.put(segIdx, part)
		}
		r.mu.Unlock()
		if err != nil {
			log.Printf("[usenet-stream] read-ahead segment %d abandoned: %v", segIdx, err)
		}
	}()
}

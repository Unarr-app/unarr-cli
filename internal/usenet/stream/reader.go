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

// maxLocateFetches bounds how many articles ONE Read may fetch while homing in
// on the article that covers the read position. Every Usenet article we pull is
// BILLED BY VOLUME, so the locate probe must never be allowed to walk the file.
//
// Before this cap the probe stepped one segment at a time from the OffsetIndex's
// *estimated* position and never re-Locate()d after Observe(), so a Seek far into
// the file cost (estimate error) / (article size) fetches: the NZB's Segment.Bytes
// are yEnc-ENCODED sizes, ~3-7% larger than the decoded bytes, so on a 10 GB
// release that is ~900 articles ≈ 670 MB of pure waste for a single tail seek —
// with no player attached (the head/tail warm-up does exactly this seek).
// articleForOffset now re-Locates after each Observe and only accepts candidates
// that move toward the target, which converges in ~2 fetches (5 measured on the
// nastiest non-uniform fixture), so this cap only fires on a pathologically
// irregular posting.
//
// Sized with room to spare, deliberately. Hitting it is NOT a graceful
// degradation: during playback the error surfaces out of Reader.Read with
// http.ServeContent already writing the response, so the video truncates
// mid-stream — the fallback to a batch download only exists at plan time. The old
// unbounded walk always landed eventually; a cap that is merely "usually enough"
// would trade hundreds of wasted MB for dead playback. 16 articles is ~12 MB
// worst case, still ~50x cheaper than the walk this replaced.
const maxLocateFetches = 16

// Readahead budget. Expressed in BYTES rather than a raw article count so the
// buffer carried ahead of the read cursor stays bounded no matter how large the
// poster's articles are, and so the cost is legible in the unit we are billed in.
// Package vars (not consts) so a caller/test can retune without a rebuild.
var (
	// ReadaheadBytes caps the cushion prefetched ahead of the read cursor. It is a
	// CEILING, not the operating point: at the ~750 KB articles a typical posting
	// uses, defaultReadaheadK (4 articles ≈ 3 MB) is what actually sizes the
	// cushion and this never binds. It binds only on postings with unusually large
	// articles, where a fixed article count would silently mean tens of MB
	// prefetched ahead of a player that may stop watching — bytes we would be
	// billed for and throw away.
	ReadaheadBytes int64 = 8 << 20
	// ReadaheadMaxArticles caps the prefetch fan-out regardless of ReadaheadBytes,
	// so a posting with tiny articles cannot spawn hundreds of goroutines.
	ReadaheadMaxArticles = 8
	// ReadaheadIdleWindow is how long after the last Read a queued prefetch stays
	// worth issuing. A prefetch goroutine parked on the NNTP connection pool while
	// the consumer disconnects would otherwise still pull (and bill) its article;
	// past this window it is dropped instead. "Cease in seconds", not in minutes.
	ReadaheadIdleWindow = 2 * time.Second
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

	mu       sync.Mutex     // guards cache + inflight + lastRead
	cache    *articleCache  // decoded articles by segment index
	inflight map[int]bool   // segments a read-ahead goroutine is currently fetching
	wg       sync.WaitGroup // tracks read-ahead goroutines (drained by Close)
	lastRead time.Time      // when the consumer last called Read (gates prefetch)

	readaheadK     int
	readaheadBytes int64
	idleWindow     time.Duration
	maxAttempts    int
	retryBackoff   time.Duration

	// budget, when set, is a hard ceiling on the NNTP bytes this reader may pull.
	// Used by speculative reads that run with NO player attached (the cold-buffer
	// warm-up); nil for live playback. Shared by pointer across readers.
	budget *FetchBudget
}

// SetFetchBudget caps the NNTP bytes this reader may pull. Implements
// BudgetedReader. Pass nil for unbounded (the default for live playback).
//
// Must be called before the first Read: from then on the prefetch goroutines read
// r.budget, and this write is not synchronised with them. Every caller applies it
// to a freshly opened reader, which is inside that window.
func (r *Reader) SetFetchBudget(b *FetchBudget) { r.budget = b }

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
		ctx:            cctx,
		cancel:         cancel,
		fetcher:        fetcher,
		ix:             ix,
		cache:          newArticleCache(defaultCacheCap),
		inflight:       make(map[int]bool),
		lastRead:       time.Now(),
		readaheadK:     defaultReadaheadK,
		readaheadBytes: ReadaheadBytes,
		idleWindow:     ReadaheadIdleWindow,
		maxAttempts:    defaultMaxAttempts,
		retryBackoff:   defaultRetryBackoff,
	}
}

// DisableReadahead turns off the prefetch cushion for a Reader that does small
// RANDOM reads rather than sequential playback — the RAR header probe, which
// reads a few hundred bytes at the front of each volume. Prefetching 4 articles
// (~3 MB) per volume there is pure billed waste: on a 99-volume release the probe
// alone pulled ~300 MB of articles the parser never looks at.
func (r *Reader) DisableReadahead() { r.readaheadK = 0 }

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
	r.mu.Lock()
	r.lastRead = time.Now()
	r.mu.Unlock()
	r.triggerReadahead(segIdx, len(part.Data))
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
// and CONVERGES: every fetched part is Observed, which re-anchors the whole
// OffsetIndex (a single observation pins a uniform posting byte-exactly), and the
// next candidate comes from a fresh Locate on the sharpened map — not from a
// one-segment step. That is what keeps a far seek at ~2 article fetches instead of
// walking hundreds of billed articles across the estimate's error (see
// maxLocateFetches). Stepping ±1 remains only as the last-resort nudge for an
// irregular posting where Locate keeps returning a segment that does not cover
// pos. io.EOF is returned when pos is past the last article's real data.
func (r *Reader) articleForOffset(pos int64) (*yenc.Part, int, error) {
	n := r.ix.SegmentCount()
	if n == 0 {
		return nil, 0, io.EOF
	}
	segIdx, _, _, ok := r.ix.Locate(pos)
	if !ok {
		return nil, 0, io.EOF
	}
	tried := make(map[int]bool, maxLocateFetches)
	for fetches := 0; fetches < maxLocateFetches; fetches++ {
		part, err := r.fetchArticle(segIdx)
		if err != nil {
			return nil, segIdx, err
		}
		r.ix.Observe(segIdx, part)
		dir := classifyOffset(pos, part)
		if dir == 0 {
			return part, segIdx, nil
		}
		tried[segIdx] = true
		next, err := r.nextCandidate(pos, segIdx, n, dir, tried)
		if err != nil {
			return nil, segIdx, err
		}
		segIdx = next
	}
	// Converging normally takes 2 fetches; hitting the cap means the posting's
	// offset map is not self-consistent. Fail cleanly — the caller falls back to
	// the batch download — rather than keep pulling billed articles.
	return nil, segIdx, fmt.Errorf(
		"usenet reader: could not locate article for offset %d within %d fetches (%d segments)",
		pos, maxLocateFetches, n)
}

// nextCandidate picks the segment to fetch next after `from` failed to cover pos.
// dir is classifyOffset's verdict for the part just fetched (-1: pos sits before
// it, +1: at/after its end). It prefers a fresh Locate on the just-sharpened index
// — the convergent path that collapses a far seek to ~2 fetches — and falls back
// to a ±1 step when Locate has nothing better to offer.
//
// The candidate must MOVE TOWARD pos. classifyOffset just proved, against the
// article's real yEnc range, that pos lies on the `dir` side of `from`; a Locate
// proposing the other side contradicts that hard evidence, so following it can
// only spend a fetch without closing the distance.
//
// This is insurance, not a measured win: on every fixture here the walk costs the
// same with it and without it, because a healthy index does not propose backwards
// candidates. It is cheap, and it upgrades "the walk usually advances" to "the
// walk cannot spend a fetch going the wrong way" — worth having on a loop whose
// budget running out truncates someone's video.
func (r *Reader) nextCandidate(pos int64, from, n, dir int, tried map[int]bool) (int, error) {
	if seg, _, _, ok := r.ix.Locate(pos); ok && !tried[seg] &&
		((dir > 0 && seg > from) || (dir < 0 && seg < from)) {
		return seg, nil
	}
	if dir < 0 {
		if from == 0 {
			return 0, fmt.Errorf("usenet reader: offset %d precedes first article", pos)
		}
		return from - 1, nil
	}
	if from >= n-1 {
		return 0, io.EOF
	}
	return from + 1, nil
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
	// Cost ceiling BEFORE the wire: a speculative reader (cold-buffer warm-up, no
	// player attached) stops pulling once it has spent its byte budget, however
	// much wall clock is left. Usenet is billed by volume, so this is the bound
	// that actually protects the account.
	if r.budget.exhausted() {
		return nil, ErrFetchBudgetExhausted
	}
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
			// Charge what came off the wire, whether or not it decodes — a corrupt
			// body was still transferred and still billed.
			r.budget.charge(int64(len(raw)))
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

// triggerReadahead prefetches the articles following fromSeg so the next
// sequential reads hit the cache. The window is a BYTE budget (readaheadBytes)
// converted to an article count using the size of the article just served, then
// clamped by readaheadK/ReadaheadMaxArticles — so the cushion carried ahead of the
// cursor stays bounded in the unit we are billed in, whatever the poster's article
// size. Message-ids are resolved here on the read goroutine; the spawned
// goroutines never touch the index.
func (r *Reader) triggerReadahead(fromSeg, articleBytes int) {
	k := r.readaheadWindow(articleBytes)
	if k <= 0 {
		return
	}
	n := r.ix.SegmentCount()
	for j := fromSeg + 1; j <= fromSeg+k && j < n; j++ {
		r.prefetch(j, r.ix.Segment(j).MessageID)
	}
}

// readaheadWindow returns how many articles may be prefetched: the byte budget
// divided by the observed article size, capped by the reader's article-count
// limit. A zero/unknown article size falls back to the count limit alone.
func (r *Reader) readaheadWindow(articleBytes int) int {
	k := r.readaheadK
	if k <= 0 {
		return 0
	}
	if k > ReadaheadMaxArticles {
		k = ReadaheadMaxArticles
	}
	if r.readaheadBytes > 0 && articleBytes > 0 {
		byBytes := int(r.readaheadBytes / int64(articleBytes))
		if byBytes < k {
			k = byBytes
		}
	}
	return k
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
		// A prefetch parked behind a busy NNTP pool can start long after the
		// consumer went away (player paused, closed, or the HTTP connection was
		// cut). Issuing it anyway pulls an article nobody will read and we get
		// BILLED for it, so drop it once the reader has been quiet: fetching must
		// cease within seconds of the last Read, not at connection teardown.
		if r.readerIdle() {
			r.mu.Lock()
			delete(r.inflight, segIdx)
			r.mu.Unlock()
			return
		}
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

// readerIdle reports whether the consumer has stopped reading for longer than the
// idle window, in which case a queued prefetch is no longer worth its billed
// bytes. A non-positive window disables the check.
func (r *Reader) readerIdle() bool {
	if r.idleWindow <= 0 {
		return false
	}
	r.mu.Lock()
	last := r.lastRead
	r.mu.Unlock()
	return time.Since(last) > r.idleWindow
}

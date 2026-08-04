package cmd

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/stream"
)

// readerSummary records where one fake reader started (its seek target) and how
// many bytes it drained — enough to assert head vs tail warm-up.
type readerSummary struct{ start, total int64 }

// fakeStreamProvider is a minimal engine.FileProvider that records, per opened
// reader, the first offset read and the total bytes drained. An optional per-Read
// delay simulates a slow NNTP so the best-effort timeout can be exercised.
type fakeStreamProvider struct {
	size  int64
	delay time.Duration

	mu       sync.Mutex
	opened   int
	summarie []readerSummary
}

func (p *fakeStreamProvider) FileName() string { return "movie.mkv" }
func (p *fakeStreamProvider) FileSize() int64  { return p.size }
func (p *fakeStreamProvider) NewFileReader(ctx context.Context) io.ReadSeekCloser {
	p.mu.Lock()
	p.opened++
	p.mu.Unlock()
	return &fakeStreamReader{p: p, ctx: ctx}
}

type fakeStreamReader struct {
	p       *fakeStreamProvider
	ctx     context.Context
	pos     int64
	started bool
	first   int64
	total   int64
	budget  *stream.FetchBudget
}

// SetFetchBudget makes the fake a stream.BudgetedReader. warmRange REFUSES to
// drain a reader it cannot byte-cap (an uncappable speculative read is exactly
// what made a stream bind cost ~1 GB), so the fake must support it like the real
// Usenet readers do.
func (r *fakeStreamReader) SetFetchBudget(b *stream.FetchBudget) { r.budget = b }

func (r *fakeStreamReader) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		r.pos = off
	case io.SeekCurrent:
		r.pos += off
	case io.SeekEnd:
		r.pos = r.p.size + off
	}
	return r.pos, nil
}

func (r *fakeStreamReader) Read(b []byte) (int, error) {
	if r.p.delay > 0 {
		select {
		case <-time.After(r.p.delay):
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		}
	}
	if r.pos >= r.p.size {
		return 0, io.EOF
	}
	if !r.started {
		r.first = r.pos
		r.started = true
	}
	n := int64(len(b))
	if rem := r.p.size - r.pos; n > rem {
		n = rem
	}
	r.pos += n
	r.total += n
	return int(n), nil
}

func (r *fakeStreamReader) Close() error {
	if r.started {
		r.p.mu.Lock()
		r.p.summarie = append(r.p.summarie, readerSummary{start: r.first, total: r.total})
		r.p.mu.Unlock()
	}
	return nil
}

// TestPrewarmUsenetHeadTailWarmsBothEnds is the cold-buffer regression guard: the
// warm-up must fetch BOTH the front (MKV header) and the tail (Cues/SeekHead) so
// the first player open finds them hot.
func TestPrewarmUsenetHeadTailWarmsBothEnds(t *testing.T) {
	const size = 100 << 20 // 100 MiB
	const head = 4 << 20
	const tail = 2 << 20
	p := &fakeStreamProvider{size: size}

	hn, tn, _ := prewarmUsenetHeadTail(context.Background(), p, head, tail, 5*time.Second)

	if hn != head {
		t.Errorf("head bytes warmed = %d, want %d", hn, head)
	}
	if tn != tail {
		t.Errorf("tail bytes warmed = %d, want %d", tn, tail)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.opened != 2 {
		t.Fatalf("opened %d readers, want 2 (head + tail)", p.opened)
	}
	var sawHead, sawTail bool
	for _, s := range p.summarie {
		if s.start == 0 && s.total == head {
			sawHead = true
		}
		if s.start == size-tail && s.total == tail {
			sawTail = true
		}
	}
	if !sawHead {
		t.Error("no reader warmed the HEAD range [0, headBytes)")
	}
	if !sawTail {
		t.Errorf("no reader warmed the TAIL range [size-tailBytes, size) — VLC's Cues/SeekHead would stall")
	}
}

// TestPrewarmUsenetHeadTailBestEffortTimeout ensures a slow NNTP never turns the
// warm-up into a hang: with a per-Read delay far larger than the timeout, prewarm
// returns promptly (best-effort) rather than blocking the ready report.
func TestPrewarmUsenetHeadTailBestEffortTimeout(t *testing.T) {
	p := &fakeStreamProvider{size: 100 << 20, delay: 10 * time.Second}

	done := make(chan time.Duration, 1)
	go func() {
		_, _, dur := prewarmUsenetHeadTail(context.Background(), p, 4<<20, 2<<20, 150*time.Millisecond)
		done <- dur
	}()

	select {
	case dur := <-done:
		if dur > 2*time.Second {
			t.Errorf("warm-up took %s despite a 150ms timeout — it must fail fast, not hang", dur)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("prewarmUsenetHeadTail did not return — a slow NNTP hung the ready report")
	}
}

// TestPrewarmUsenetHeadTailRespectsCancel ensures a per-task cancel (fix #1) aborts
// the warm-up instead of leaving it running.
func TestPrewarmUsenetHeadTailRespectsCancel(t *testing.T) {
	p := &fakeStreamProvider{size: 100 << 20, delay: 10 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		prewarmUsenetHeadTail(ctx, p, 4<<20, 2<<20, 30*time.Second)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("warm-up ignored ctx cancel — must abort on a per-task cancel")
	}
}

// TestPrewarmUsenetHeadTailTinyFile: a file smaller than the tail window warms only
// the head (no negative/overlapping tail range).
func TestPrewarmUsenetHeadTailTinyFile(t *testing.T) {
	const size = 1 << 20 // 1 MiB, smaller than the 2 MiB tail window
	p := &fakeStreamProvider{size: size}

	hn, tn, _ := prewarmUsenetHeadTail(context.Background(), p, 4<<20, 2<<20, 5*time.Second)

	if hn != size {
		t.Errorf("head warmed = %d, want the whole %d-byte file", hn, size)
	}
	if tn != 0 {
		t.Errorf("tail warmed = %d, want 0 for a file smaller than the tail window", tn)
	}
}

// uncappableReader is a stream reader that does NOT implement
// stream.BudgetedReader — the shape warmRange must refuse.
type uncappableReader struct{ pos, size int64 }

func (r *uncappableReader) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		r.pos = off
	case io.SeekCurrent:
		r.pos += off
	case io.SeekEnd:
		r.pos = r.size + off
	}
	return r.pos, nil
}

func (r *uncappableReader) Read(b []byte) (int, error) {
	if r.pos >= r.size {
		return 0, io.EOF
	}
	n := int64(len(b))
	if rem := r.size - r.pos; n > rem {
		n = rem
	}
	r.pos += n
	return int(n), nil
}
func (r *uncappableReader) Close() error { return nil }

type uncappableProvider struct{ size int64 }

func (p *uncappableProvider) FileName() string { return "movie.mkv" }
func (p *uncappableProvider) FileSize() int64  { return p.size }
func (p *uncappableProvider) NewFileReader(context.Context) io.ReadSeekCloser {
	return &uncappableReader{size: p.size}
}

// TestPrewarmRefusesUncappableReader is the cost fail-safe: the warm-up drains
// bytes with NO player attached, so a reader whose NNTP spend cannot be bounded
// must NOT be warmed at all. Warming an uncappable reader is exactly how a stream
// bind came to cost ~1 GB — better a cold first open (a latency regression the
// player recovers from) than an unbounded, billed drain.
func TestPrewarmRefusesUncappableReader(t *testing.T) {
	p := &uncappableProvider{size: 100 << 20}

	hn, tn, _ := prewarmUsenetHeadTail(context.Background(), p, 4<<20, 2<<20, 5*time.Second)

	if hn != 0 || tn != 0 {
		t.Fatalf("warmed head=%d tail=%d from a reader that cannot be byte-capped; want 0/0", hn, tn)
	}
}

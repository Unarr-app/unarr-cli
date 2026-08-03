package stream

// Cost invariants for the on-the-fly Usenet stream path.
//
// Usenet is billed BY VOLUME (~€0.015/GB), so "it works" is not the bar: every
// article we pull and throw away is money. These tests pin, against the in-memory
// fake NNTP server, the three ways this path used to overfetch:
//
//  1. a far seek walked the OffsetIndex's estimate error one 750 KB article at a
//     time (the head/tail warm-up does exactly this seek, with NO player attached);
//  2. the RAR header probe carried a sequential-playback read-ahead cushion it
//     never reads, ~3 MB per volume;
//  3. nothing anywhere was bounded in BYTES — only in wall clock, which at
//     ~55 MB/s is not a bound at all.

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nzb"
)

// newCountingReader wires a Reader to a fake NNTP server over a uniform posting,
// returning the reader and the server so BodyCalls can be asserted. Read-ahead is
// left at the production default so the assertions cover the real cost.
func newCountingReader(t *testing.T, parts, partSize int) (*Reader, *nntptest.FakeServer, []byte) {
	t.Helper()
	content := patternBytes(parts*partSize + 123)
	n, articles := nntptest.BuildDirectFile("movie.mkv", content, partSize)
	s := nntptest.NewFakeServer(t)
	s.AddArticles(articles)
	c := dialFake(t, s)

	f := n.Files[0]
	r := NewReader(context.Background(), c, f, NewOffsetIndex(f))
	r.retryBackoff = time.Millisecond
	t.Cleanup(func() { _ = r.Close() })
	return r, s, content
}

// TestSeekToTailCostsBoundedArticles is THE regression guard for the overfetch.
//
// The NZB's Segment.Bytes are yEnc-ENCODED sizes, ~3-7% larger than the decoded
// bytes, so a fresh OffsetIndex places offsets several percent too far along. The
// old locate probe stepped ONE segment at a time from that estimate and never
// re-Locate()d after Observe() had already re-anchored the index — so a seek to
// the tail cost (estimate error / article size) fetches: measured at 26 articles
// on a 400-part fixture, which extrapolates to ~900 articles ≈ 670 MB on a 10 GB
// release, pulled with nobody watching.
//
// Locating must now CONVERGE: one fetch to anchor the index, one to land.
func TestSeekToTailCostsBoundedArticles(t *testing.T) {
	const partSize = 4096
	const parts = 400
	r, s, content := newCountingReader(t, parts, partSize)

	// Sanity: the fixture must actually carry the estimate error this guards.
	est := NewOffsetIndex(mustFile(t, "movie.mkv", content, partSize)).FileSize()
	if est <= int64(len(content)) {
		t.Fatalf("fixture has no estimate error (est %d <= exact %d) — this test would pass vacuously",
			est, len(content))
	}

	// Count the locate's OWN fetches: with the cushion on, read-ahead goroutines
	// race into the same window and land in BodyCalls, which made this assertion
	// intermittent (2 spurious failures in 200 runs) for a reason that has nothing
	// to do with what it guards.
	r.readaheadK = 0

	// Exactly what warmRange(tail) does: seek deep into the file on a FRESH reader
	// whose index is still an estimate, then read.
	if _, err := r.Seek(int64(len(content))-8*partSize, io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	before := s.BodyCalls()
	buf := make([]byte, 1024)
	if _, err := r.Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	cost := s.BodyCalls() - before

	if cost > maxLocateFetches {
		t.Fatalf("tail seek cost %d article fetches, cap is %d — the locate probe is walking the file again",
			cost, maxLocateFetches)
	}
	// Tight bound: convergence is 2 fetches (anchor + land). Allow a little slack
	// for read-ahead racing into the same window, but not a walk.
	if cost > 4 {
		t.Errorf("tail seek cost %d article fetches, want <= 4 (converged locate) — %d parts in file",
			cost, parts)
	}
	t.Logf("tail seek cost: %d article fetches over a %d-part file", cost, parts)
}

// TestSeekedReadReturnsCorrectBytes guards that the cheaper locate is still
// CORRECT — a bounded probe that returns the wrong article would be far worse
// than an expensive one.
func TestSeekedReadReturnsCorrectBytes(t *testing.T) {
	const partSize = 4096
	const parts = 120
	r, _, content := newCountingReader(t, parts, partSize)

	for _, off := range []int64{0, 1, int64(partSize) - 1, int64(partSize), 7*int64(partSize) + 33,
		int64(len(content)) - int64(partSize), int64(len(content)) - 1} {
		if _, err := r.Seek(off, io.SeekStart); err != nil {
			t.Fatalf("seek %d: %v", off, err)
		}
		buf := make([]byte, 64)
		n, err := r.Read(buf)
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("read at %d: %v", off, err)
		}
		want := content[off:min(off+int64(n), int64(len(content)))]
		if string(buf[:n]) != string(want) {
			t.Fatalf("read at offset %d returned wrong bytes (%d read)", off, n)
		}
	}
}

// TestFetchBudgetStopsSpeculativeDrain pins the byte ceiling: a reader given a
// budget must stop pulling from NNTP once it is spent, no matter how much the
// caller asks for. This is the bound that protects the account when the wall
// clock does not — at ~55 MB/s even a 5 s window is ~275 MB.
func TestFetchBudgetStopsSpeculativeDrain(t *testing.T) {
	const partSize = 4096
	const parts = 300
	r, s, _ := newCountingReader(t, parts, partSize)
	r.readaheadK = 0 // isolate the read path from prefetch for an exact count

	// Budget ~5 articles' worth of wire bytes.
	const budgetBytes = 5 * partSize
	r.SetFetchBudget(NewFetchBudget(budgetBytes))

	// Ask for the WHOLE file; the budget must cut it short.
	drained, err := io.Copy(io.Discard, r)
	if err != nil && !errors.Is(err, ErrFetchBudgetExhausted) {
		t.Fatalf("drain ended with %v, want budget exhaustion (or clean end)", err)
	}
	if drained >= int64(parts*partSize) {
		t.Fatalf("drained %d bytes — the budget did not bound the read at all", drained)
	}
	// Wire bytes are yEnc-encoded, so allow the overshoot of the one article that
	// crossed the line, but the order of magnitude must hold.
	if calls := s.BodyCalls(); calls > 8 {
		t.Errorf("budget of %d bytes still cost %d article fetches — cap not enforced at the fetch",
			budgetBytes, calls)
	}
	t.Logf("budget %d bytes -> %d article fetches, %d bytes drained", budgetBytes, s.BodyCalls(), drained)
}

// TestFetchBudgetNilIsUnbounded guards that live playback (no budget) is NOT
// throttled — the cost controls must never starve a real viewer.
func TestFetchBudgetNilIsUnbounded(t *testing.T) {
	const partSize = 1024
	const parts = 20
	r, _, content := newCountingReader(t, parts, partSize)
	r.SetFetchBudget(nil)

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != len(content) {
		t.Fatalf("streamed %d bytes, want the whole %d — an unbudgeted reader must not be capped",
			len(got), len(content))
	}
}

// TestReadaheadWindowIsByteBounded pins the cushion carried ahead of the read
// cursor: it is a BYTE budget, so a posting with large articles prefetches fewer
// of them rather than blindly pulling readaheadK × article size.
func TestReadaheadWindowIsByteBounded(t *testing.T) {
	r, _, _ := newCountingReader(t, 4, 1024)

	tests := []struct {
		name           string
		readaheadBytes int64
		readaheadK     int
		articleBytes   int
		want           int
	}{
		{"budget allows the full article window", 8 << 20, 4, 750 << 10, 4},
		{"budget narrower than the window clamps it", 2 << 20, 4, 750 << 10, 2},
		{"one huge article exceeds the budget entirely", 1 << 20, 4, 4 << 20, 0},
		{"tiny articles are capped by the article limit", 64 << 20, 4, 1 << 10, 4},
		{"article limit caps an over-large K", 64 << 20, 999, 1 << 10, ReadaheadMaxArticles},
		{"read-ahead disabled stays disabled", 8 << 20, 0, 750 << 10, 0},
		{"unknown article size falls back to the count", 8 << 20, 4, 0, 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r.readaheadBytes = tc.readaheadBytes
			r.readaheadK = tc.readaheadK
			if got := r.readaheadWindow(tc.articleBytes); got != tc.want {
				t.Errorf("readaheadWindow(%d) with budget %d / K %d = %d, want %d",
					tc.articleBytes, tc.readaheadBytes, tc.readaheadK, got, tc.want)
			}
		})
	}
}

// TestIdleReaderDropsQueuedPrefetch pins "when nobody is reading, fetching stops
// in SECONDS". A prefetch parked behind a busy NNTP pool must be dropped once the
// consumer has gone quiet rather than pulling an article nobody will read.
func TestIdleReaderDropsQueuedPrefetch(t *testing.T) {
	r, s, _ := newCountingReader(t, 50, 1024)
	r.idleWindow = 10 * time.Millisecond

	// Pretend the last Read was long ago (player closed / paused).
	r.mu.Lock()
	r.lastRead = time.Now().Add(-time.Hour)
	r.mu.Unlock()

	before := s.BodyCalls()
	r.prefetch(10, r.ix.Segment(10).MessageID, r.ix.Segment(10).Bytes)
	r.wg.Wait()

	if got := s.BodyCalls() - before; got != 0 {
		t.Fatalf("an idle reader still issued %d prefetch fetch(es) — read-ahead must cease when nobody reads", got)
	}

	// And it must resume the moment a reader is active again.
	r.mu.Lock()
	r.lastRead = time.Now()
	r.mu.Unlock()
	r.prefetch(11, r.ix.Segment(11).MessageID, r.ix.Segment(11).Bytes)
	r.wg.Wait()
	if got := s.BodyCalls() - before; got != 1 {
		t.Fatalf("an ACTIVE reader issued %d prefetch fetch(es), want 1 — playback must keep its cushion", got)
	}
}

// TestProbeDoesNotPrefetchPastHeaders pins the header-probe cost: classifying a
// RAR release must fetch only the article(s) the parser actually reads, NOT the
// sequential-playback read-ahead cushion.
//
// The probe does small random reads at the front of each volume, so every
// prefetched article is pure waste — and it is waste paid BEFORE we even know the
// release is streamable. At the production ~750 KB article size the old cushion
// cost ~3 MB per volume: ~300 MB of billed traffic to classify a 99-volume
// release that might immediately fall back to the batch download anyway.
func TestProbeDoesNotPrefetchPastHeaders(t *testing.T) {
	// Many small volumes with several articles each, so a per-volume read-ahead
	// cushion would be plainly visible in the fetch count.
	const volSize, partSize = 6000, 400
	content := patternBytes(60_000)
	n, articles := nntptest.BuildRarStore("show.s01e01.mkv", content, volSize, partSize)

	s := nntptest.NewFakeServer(t)
	s.AddArticles(articles)
	c := dialFake(t, s)

	volumes := n.RarFiles()
	if len(volumes) < 4 {
		t.Fatalf("fixture built %d volumes, need several for this to be meaningful", len(volumes))
	}

	if _, err := Probe(context.Background(), c, volumes); err != nil {
		t.Fatalf("Probe: %v", err)
	}

	// Budget: the parser needs the volume's first article, plus occasionally one
	// more for a header that straddles an article boundary — measured at 1.1/volume.
	//
	// This has to be TIGHT. At 3/volume the threshold sat above the ~2.3-2.9/volume
	// the probe costs with the cushion back on, so the test passed with the fix
	// reverted and guarded nothing. 2 leaves room for the straddle case and still
	// fails immediately if the read-ahead returns.
	const perVolumeBudget = 2
	if got, want := s.BodyCalls(), len(volumes)*perVolumeBudget; got > want {
		t.Fatalf("header probe of %d volume(s) cost %d article fetches, want <= %d (~%d/volume) — "+
			"read-ahead is back on the probe readers",
			len(volumes), got, want, perVolumeBudget)
	}
	t.Logf("probe cost: %d article fetches over %d volumes (%.1f/volume)",
		s.BodyCalls(), len(volumes), float64(s.BodyCalls())/float64(len(volumes)))
}

// TestRarPlaybackKeepsReadahead is the counterweight to the test above: the
// cushion must be removed from the PROBE only. A real viewer streaming out of a
// RAR still needs read-ahead, or the fix would trade money for rebuffering.
func TestRarPlaybackKeepsReadahead(t *testing.T) {
	content := patternBytes(20_000)
	n, articles := nntptest.BuildRarStore("show.s01e01.mkv", content, 7000, 1200)
	s := nntptest.NewFakeServer(t)
	s.AddArticles(articles)
	c := dialFake(t, s)

	rs, err := Probe(context.Background(), c, n.RarFiles())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	rd := rs.OpenVideo(context.Background())
	defer rd.Close()

	rv, ok := rd.(*rarVideoReader)
	if !ok {
		t.Fatalf("OpenVideo returned %T, want *rarVideoReader", rd)
	}
	buf := make([]byte, 64)
	if _, err := rv.Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if rv.curReader == nil {
		t.Fatal("no volume reader opened")
	}
	if rv.curReader.r.readaheadK <= 0 {
		t.Error("playback volume reader has read-ahead disabled — a viewer would rebuffer")
	}
}

// TestRarPlaybackBudgetSurvivesVolumeSwitch guards that a budgeted speculative
// read (the warm-up) cannot reset its cost by crossing a volume boundary: each
// boundary mints a FRESH per-volume Reader, and an unpropagated budget there
// would make the ceiling meaningless on any multi-volume release.
func TestRarPlaybackBudgetSurvivesVolumeSwitch(t *testing.T) {
	content := patternBytes(40_000)
	n, articles := nntptest.BuildRarStore("show.s01e01.mkv", content, 6000, 1000)
	s := nntptest.NewFakeServer(t)
	s.AddArticles(articles)
	c := dialFake(t, s)

	rs, err := Probe(context.Background(), c, n.RarFiles())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	rd := rs.OpenVideo(context.Background())
	defer rd.Close()

	budget := NewFetchBudget(4000) // a few articles' worth
	if !ApplyFetchBudget(rd, budget) {
		t.Fatal("rarVideoReader does not implement BudgetedReader — warmRange would refuse to warm it")
	}

	drained, err := io.Copy(io.Discard, rd)
	if err != nil && !errors.Is(err, ErrFetchBudgetExhausted) {
		t.Fatalf("drain ended with %v", err)
	}
	if drained >= int64(len(content)) {
		t.Fatalf("drained the whole %d-byte video despite a 4000-byte budget — "+
			"the ceiling is lost at the volume boundary", len(content))
	}
	t.Logf("budgeted rar drain stopped after %d bytes (spent %d on the wire)", drained, budget.Spent())
}

// TestLocateConvergesOnNonUniformPostings is the correctness counterweight to
// TestSeekToTailCostsBoundedArticles: making locate CHEAP must not make it FAIL.
//
// Every other fixture here is uniform, where one Observe pins the whole layout
// byte-exactly and any locate strategy lands. Real postings are not: a poster
// tops off with a short final part, resumes an interrupted upload at a different
// part size, or reposts a repaired set. Two ways a converging locate can lose a
// seek that the old one-segment walk would have found:
//
//   - it accepts a Locate candidate pointing AWAY from the target, so the walk
//     ping-pongs instead of advancing and burns the fetch cap without progress;
//   - the index infers one global part size from the first article it sees, which
//     for an atypical first article poisons the map for every unobserved segment.
//
// A failed locate is not a degraded stream, it is a dead one: in playback the
// error surfaces from Reader.Read with http.ServeContent already streaming, so
// the response truncates mid-video (there is no fallback to batch there — that
// only exists at plan time). This sweeps offsets across the file and demands both
// the right bytes and a bounded cost.
func TestLocateConvergesOnNonUniformPostings(t *testing.T) {
	cases := []struct {
		name  string
		sizes []int
	}{
		{
			// Jitter around a nominal part size: no single step length describes
			// the file, so an inferred uniform step drifts further with every
			// segment it is applied to.
			name:  "jittered part sizes",
			sizes: cyclePartSizes([]int{4096, 2560, 5888, 3712}, 120),
		},
		{
			// The first article is the one the tail seek observes first. If the
			// index latches ITS length as the file's step, every unobserved
			// segment is placed with a 8x error.
			name:  "atypical first article",
			sizes: append([]int{512}, cyclePartSizes([]int{4096}, 119)...),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			total := 0
			for _, s := range tc.sizes {
				total += s
			}
			content := patternBytes(total)
			n, articles := nntptest.BuildDirectFileParts("movie.mkv", content, tc.sizes)
			s := nntptest.NewFakeServer(t)
			s.AddArticles(articles)
			c := dialFake(t, s)

			f := n.Files[0]
			worst, failures := 0, 0
			// A FRESH reader per offset is the honest case: that is what every new
			// stream session gets. Reusing one reader would let each seek land next
			// to a segment the previous seek already anchored, which is precisely
			// the easy case that hides this class of bug.
			for i := len(tc.sizes) - 1; i >= 0; i -= 7 {
				off := int64(0)
				for j := 0; j < i; j++ {
					off += int64(tc.sizes[j])
				}
				off += int64(tc.sizes[i] / 2) // mid-article, so a ±1 error is visible

				r := NewReader(context.Background(), c, f, NewOffsetIndex(f))
				r.retryBackoff = time.Millisecond
				r.readaheadK = 0 // count the locate's own fetches, not the cushion's

				// Seek from the END, like http.ServeContent and openReaderVolume do.
				// That routes through ensureSizeExact, which observes segment 0 —
				// so segment 0's length is the first (and for a while only) evidence
				// the index has about how this posting is laid out.
				if _, err := r.Seek(off-int64(len(content)), io.SeekEnd); err != nil {
					t.Fatalf("seek to %d: %v", off, err)
				}
				before := s.BodyCalls()
				buf := make([]byte, 64)
				err := readFullFrom(r, buf)
				cost := s.BodyCalls() - before
				if cost > worst {
					worst = cost
				}
				if err != nil {
					failures++
					t.Errorf("segment %d/%d: read at offset %d failed after %d fetches: %v",
						i, len(tc.sizes), off, cost, err)
					_ = r.Close()
					continue
				}
				if got, want := buf, content[off:off+64]; string(got) != string(want) {
					t.Errorf("segment %d: wrong bytes at offset %d", i, off)
				}
				_ = r.Close()
			}
			if failures > 0 {
				t.Fatalf("%d offsets could not be located on a non-uniform posting — "+
					"a converging locate must never lose a seek the one-segment walk would have found",
					failures)
			}
			t.Logf("worst locate cost: %d article fetches over %d non-uniform parts",
				worst, len(tc.sizes))
		})
	}
}

// TestProbeStopsAtItsBudgetAndFallsBack pins that classifying a release cannot
// walk it without a ceiling.
//
// The probe is the most speculative traffic this path issues: it runs before
// anything is known to be playable, with no player attached, fanned out across
// the pool. Its normal cost (one article per volume) is inherent and stays; what
// must not be open-ended is the tail — a container whose headers straddle
// articles, or an NZB whose declared sizes bear no relation to what arrives.
//
// Running out must land on the ordinary not-streamable path (the batch download),
// never a hang or a hard fault, so a bounded probe degrades exactly like an
// unreadable header does.
func TestProbeStopsAtItsBudgetAndFallsBack(t *testing.T) {
	const volSize, partSize = 6000, 400
	content := patternBytes(60_000)
	n, articles := nntptest.BuildRarStore("show.s01e01.mkv", content, volSize, partSize)

	s := nntptest.NewFakeServer(t)
	s.AddArticles(articles)
	c := dialFake(t, s)

	volumes := n.RarFiles()
	// An NZB that under-declares its articles: the budget is derived from the
	// declared sizes, the bytes that actually arrive are the real ones. One
	// article's worth of real data therefore blows a budget built from lies.
	for i := range volumes {
		for j := range volumes[i].Segments {
			volumes[i].Segments[j].Bytes = 1
		}
	}

	before := s.BodyCalls()
	_, err := Probe(context.Background(), c, volumes)
	cost := s.BodyCalls() - before

	if err == nil {
		t.Fatalf("Probe succeeded against a budget of %d bytes for %d volumes — it is not bounded",
			probeBudget(volumes).Spent(), len(volumes))
	}
	if !errors.Is(err, ErrNotStreamable) {
		t.Fatalf("Probe failed with %v, want a not-streamable outcome so the caller falls back to batch", err)
	}
	if cost >= len(volumes) {
		t.Errorf("probe spent %d fetches over %d volumes despite an exhausted budget — it kept walking",
			cost, len(volumes))
	}
	t.Logf("exhausted probe stopped after %d fetches over %d volumes: %v", cost, len(volumes), err)
}

// TestBudgetReservesBeforeTheWire pins that the ceiling holds when several
// fetches race, which is the only way it is ever used: head and tail warm-up
// goroutines plus a read-ahead fan-out all draw on ONE pot.
//
// Checking "is there room?" and debiting after the article lands is not a
// ceiling. Every racer passes the check before the first debit arrives, so N
// concurrent fetches all proceed against a budget with room for a few — the
// overspend scales with the fan-out, not with the cap. Claiming the bytes up
// front is what makes the (N+1)th fetch see the first N.
func TestBudgetReservesBeforeTheWire(t *testing.T) {
	const (
		parts      = 32
		partSize   = 4096
		concurrent = 12
		roomFor    = 3 // articles the budget can afford
	)
	content := patternBytes(parts * partSize)
	n, articles := nntptest.BuildDirectFile("movie.mkv", content, partSize)
	f := n.Files[0]

	// Every fetch parks inside Body until released, so all `concurrent` callers
	// are in flight at once — the exact window a check-then-debit budget misses.
	g := &gatedFetcher{bodies: articles, release: make(chan struct{})}

	ix := NewOffsetIndex(f)
	budget := NewFetchBudget(int64(roomFor) * ix.Segment(0).Bytes)
	r := NewReader(context.Background(), g, f, ix)
	r.retryBackoff = time.Millisecond
	r.SetFetchBudget(budget)
	defer func() { _ = r.Close() }()

	var wg sync.WaitGroup
	var refused atomic.Int64
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func(seg int) {
			defer wg.Done()
			s := ix.Segment(seg)
			if _, err := r.fetchDecodeRetry(s.MessageID, s.Bytes); err != nil {
				refused.Add(1)
			}
		}(i)
	}

	// Let everyone either enter Body or be refused, then release the parked ones.
	deadline := time.After(5 * time.Second)
	for int64(g.entered())+refused.Load() < concurrent {
		select {
		case <-deadline:
			t.Fatalf("only %d entered + %d refused of %d after 5s",
				g.entered(), refused.Load(), concurrent)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	close(g.release)
	wg.Wait()

	cap := int64(roomFor) * ix.Segment(0).Bytes
	if got := budget.Spent(); got > cap {
		t.Fatalf("budget of %d bytes (%d articles) spent %d with %d concurrent fetches — "+
			"the ceiling is being checked, not reserved", cap, roomFor, got, concurrent)
	}
	if g.entered() > roomFor {
		t.Errorf("%d fetches reached the wire against a %d-article budget", g.entered(), roomFor)
	}
	t.Logf("%d concurrent fetches against a %d-article budget: %d reached the wire, %d spent of %d",
		concurrent, roomFor, g.entered(), budget.Spent(), cap)
}

// --- helpers ---

// gatedFetcher parks every Body call until release is closed, so a test can hold
// N fetches in flight simultaneously.
type gatedFetcher struct {
	bodies  map[string][]byte
	release chan struct{}
	n       atomic.Int64
}

func (g *gatedFetcher) Body(ctx context.Context, messageID string) ([]byte, error) {
	g.n.Add(1)
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.bodies[messageID], nil
}

func (g *gatedFetcher) entered() int { return int(g.n.Load()) }

func mustFile(t *testing.T, name string, content []byte, partSize int) nzb.File {
	t.Helper()
	n, _ := nntptest.BuildDirectFile(name, content, partSize)
	return n.Files[0]
}

// readFullFrom fills buf, reporting the first error instead of panicking, so a
// locate failure is a test failure with context rather than a fatal.
func readFullFrom(r io.Reader, buf []byte) error {
	_, err := io.ReadFull(r, buf)
	return err
}

// cyclePartSizes repeats pattern until it has n sizes.
func cyclePartSizes(pattern []int, n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = pattern[i%len(pattern)]
	}
	return out
}

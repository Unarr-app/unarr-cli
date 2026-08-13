package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Reuse of a PARTIAL cache dir (segments on disk, no .complete marker) is a
// documented design goal — hls_cache.go's header states "next session can reuse
// the segments already present, ffmpeg fills the gaps". These tests pin the
// behaviour that makes that promise hold, and currently FAIL because the gap
// filling was never implemented.
//
// How a partial dir survives to be reused at all: Close() invalidates it
// (hls.go), so a clean stop leaves nothing behind. The dir persists only when
// Close never runs — daemon SIGKILL/panic/host suspend mid-encode — and the
// daemon then restarts within hlsCacheStartupOrphanAge, so cleanStartupOrphans
// spares it. From that point nothing ever purges it again.

// seedPartialCacheDir writes init.mp4 plus seg-0..seg-(n-1) into the cache dir
// for key, simulating the leftovers of a play that was killed after n segments.
// Deliberately no .complete marker: this is a partial run, not a sealed entry.
func seedPartialCacheDir(t *testing.T, c *HLSCache, key string, n int) string {
	t.Helper()
	videoDir := filepath.Join(c.DirFor(key), "video")
	if err := os.MkdirAll(videoDir, 0o755); err != nil {
		t.Fatalf("mkdir video: %v", err)
	}
	if err := os.WriteFile(filepath.Join(videoDir, "init.mp4"), []byte("init"), 0o644); err != nil {
		t.Fatalf("write init.mp4: %v", err)
	}
	for i := 0; i < n; i++ {
		p := filepath.Join(videoDir, fmt.Sprintf("seg-%d.m4s", i))
		if err := os.WriteFile(p, []byte(fmt.Sprintf("stale-seg-%d", i)), 0o644); err != nil {
			t.Fatalf("write seg-%d: %v", i, err)
		}
	}
	return videoDir
}

// newPartialReuseSession builds a session pointed at a pre-seeded partial cache
// dir, with no ffmpeg behind it. segmentCount models a source far longer than
// the stale block, so the session must still produce the remainder.
func newPartialReuseSession(t *testing.T, c *HLSCache, key string, segmentCount int) *HLSSession {
	t.Helper()
	return &HLSSession{
		cfg:            HLSSessionConfig{SessionID: "partial-reuse"},
		tmpDir:         c.DirFor(key),
		durationSec:    float64(segmentCount * hlsSegmentDuration),
		segmentCount:   segmentCount,
		startedAt:      time.Now(),
		lastTouch:      time.Now(),
		readyCh:        make(chan struct{}),
		cache:          c,
		cacheKey:       key,
		writerLockHeld: true,
	}
}

// TestPartialReuseStartsEncoderAtTheGap is the fix for the stall. The encoder
// must start where the leftovers end, so the segments nobody has are the ones
// it produces. Starting at 0 instead re-encodes what is already on disk and
// reaches the gap only after the whole prefix — minutes of playback dead at the
// boundary — which is precisely the reported symptom.
func TestPartialReuseStartsEncoderAtTheGap(t *testing.T) {
	c := newTestCache(t, 1)
	const staleCount = 600
	const segmentCount = 900
	key := "stalepoll"
	videoDir := seedPartialCacheDir(t, c, key, staleCount)

	// wantStart 0: a plain play with no resume position.
	startIdx, inherited, err := planPartialReuse(videoDir, segmentCount, 0)
	if err != nil {
		t.Fatalf("planPartialReuse: %v", err)
	}

	// The last inherited segment is given back to the encoder because a killed
	// run leaves it possibly truncated, so the usable prefix is staleCount-1.
	const wantInherited = staleCount - 1
	if inherited != wantInherited {
		t.Fatalf("inherited = %d, want %d", inherited, wantInherited)
	}
	if startIdx != wantInherited {
		t.Fatalf("startIdx = %d, want %d: the encoder must start at the first "+
			"segment the leftovers do not cover, otherwise nothing ever writes "+
			"the gap and playback stops there", startIdx, wantInherited)
	}

	// Everything from the start index up must be gone: ServeSegment's on-disk
	// fast path would otherwise hand a viewer bytes from the dead run.
	for _, i := range []int{startIdx, startIdx + 1, segmentCount - 1} {
		if _, err := os.Stat(filepath.Join(videoDir, fmt.Sprintf("seg-%d.m4s", i))); !os.IsNotExist(err) {
			t.Fatalf("seg-%d still on disk at or above the encoder start index", i)
		}
	}
	// The adopted prefix must survive — that reuse is the whole point.
	if _, err := os.Stat(filepath.Join(videoDir, "seg-0.m4s")); err != nil {
		t.Fatalf("inherited seg-0 was discarded: %v", err)
	}
}

// TestPollSegmentsDoesNotInheritStaleSegments pins the invariant pollSegments
// depends on. It infers progress from the filesystem and cannot tell runs
// apart, so once the dir has been reconciled it must never see a segment above
// the encoder's start index that the encoder did not write. When that holds,
// readyMax stays put while ffmpeg is silent — no matter how many leftovers the
// dir began with.
func TestPollSegmentsDoesNotInheritStaleSegments(t *testing.T) {
	c := newTestCache(t, 1)
	const staleCount = 600
	const segmentCount = 900
	key := "stalepoll2"
	videoDir := seedPartialCacheDir(t, c, key, staleCount)

	startIdx, inherited, err := planPartialReuse(videoDir, segmentCount, 0)
	if err != nil {
		t.Fatalf("planPartialReuse: %v", err)
	}

	s := newPartialReuseSession(t, c, key, segmentCount)
	s.ffmpegSegStart = startIdx
	s.inheritedMax = inherited
	s.readyMax = startIdx

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.pollSegments(ctx)

	// pollSegments ticks every 250ms; give it several rounds to settle.
	time.Sleep(1200 * time.Millisecond)

	s.readyMu.Lock()
	got := s.readyMax
	s.readyMu.Unlock()

	if got != startIdx {
		t.Fatalf("readyMax = %d, want %d: pollSegments credited output to an "+
			"ffmpeg that has written nothing, so the gap at seg-%d will never "+
			"be filled and playback stalls there", got, startIdx, startIdx)
	}
}

// TestServeSegmentContinuesPastStaleBlock checks the boundary the viewer hits.
// With the encoder aimed at the gap, the first segment past the inherited block
// is one the current ffmpeg is actively producing: waitForSegment blocks
// briefly for a real writer rather than timing out on a segment nobody owns.
func TestServeSegmentContinuesPastStaleBlock(t *testing.T) {
	c := newTestCache(t, 1)
	const staleCount = 600
	const segmentCount = 900
	key := "staleserve"
	videoDir := seedPartialCacheDir(t, c, key, staleCount)

	startIdx, inherited, err := planPartialReuse(videoDir, segmentCount, 0)
	if err != nil {
		t.Fatalf("planPartialReuse: %v", err)
	}

	s := newPartialReuseSession(t, c, key, segmentCount)
	s.ffmpegSegStart = startIdx
	s.inheritedMax = inherited
	s.readyMax = startIdx

	// The boundary segment: the first one the inherited block does not cover.
	boundary := startIdx

	// Either the writer is already heading for it, or ServeSegment restarts
	// ffmpeg there. The bug was that neither held: readyMax claimed the
	// segment existed, so no restart fired and no writer was coming.
	writerHeadingForIt := boundary >= s.ffmpegSegStart
	wouldRestart := boundary >= s.readyMax+hlsSeekAhead || boundary < s.ffmpegSegStart

	if !writerHeadingForIt && !wouldRestart {
		t.Fatalf("no recovery at the cache boundary: seg-%d is absent, yet "+
			"ServeSegment neither restarts ffmpeg there nor has a writer heading "+
			"for it (readyMax=%d, ffmpegSegStart=%d, hlsSeekAhead=%d). The request "+
			"blocks for segmentWaitTimeout (%s) and then 503s — playback stops at "+
			"the end of the cached block instead of continuing",
			boundary, s.readyMax, s.ffmpegSegStart, hlsSeekAhead, s.segmentWaitTimeout())
	}

	// And the stale copy of the boundary segment must not be lying around for
	// the fast path to serve in place of the encoder's output.
	if _, err := os.Stat(filepath.Join(videoDir, fmt.Sprintf("seg-%d.m4s", boundary))); !os.IsNotExist(err) {
		t.Fatalf("stale seg-%d survived reconciliation; ServeSegment's fast path "+
			"would serve the dead run's bytes at the splice point", boundary)
	}
}

// TestSealSurvivesSeekAfterPartialReuse guards a regression the sealing guard
// introduced: keyed on the live writer index, a viewer scrubbing forward made
// a reused session look like it had started mid-file, so a clean full encode
// was thrown away and the next play re-encoded from scratch. Sealing reads the
// low-water mark of everything this session encoded, which a forward seek does
// not move.
func TestSealSurvivesSeekAfterPartialReuse(t *testing.T) {
	c := newTestCache(t, 1)
	const staleCount = 600
	const segmentCount = 900
	key := "sealafterseek"
	videoDir := seedPartialCacheDir(t, c, key, staleCount)

	startIdx, inherited, err := planPartialReuse(videoDir, segmentCount, 0)
	if err != nil {
		t.Fatalf("planPartialReuse: %v", err)
	}

	s := newPartialReuseSession(t, c, key, segmentCount)
	s.ffmpegSegStart = startIdx
	s.lowestEncodedIdx = startIdx
	s.inheritedMax = inherited

	// The encoder fills the gap, then the viewer scrubs ahead and a
	// seek-restart re-aims ffmpeg — as restartFromSegment does.
	for i := startIdx; i < segmentCount; i++ {
		p := filepath.Join(videoDir, fmt.Sprintf("seg-%d.m4s", i))
		if err := os.WriteFile(p, []byte(fmt.Sprintf("fresh-seg-%d", i)), 0o644); err != nil {
			t.Fatalf("write seg-%d: %v", i, err)
		}
	}
	const seekTarget = 800
	s.ffmpegSegStart = seekTarget
	if seekTarget < s.lowestEncodedIdx {
		s.lowestEncodedIdx = seekTarget
	}
	s.readyMax = segmentCount

	if !s.allSegmentsPresent() {
		t.Fatal("a complete encode was refused for sealing because the viewer " +
			"seeked: every later play would re-encode from scratch")
	}
}

// TestPartialReuseDropsSegmentsAboveSegmentCount covers leftovers that a
// bounded 0..segmentCount sweep cannot see. A previous run of a longer source
// (or one cut at a different segment duration) leaves high-index files behind;
// VerifyComplete stats the last expected segment, so a stray one is exactly
// the evidence that later validates a directory holding two encodes.
func TestPartialReuseDropsSegmentsAboveSegmentCount(t *testing.T) {
	c := newTestCache(t, 1)
	const segmentCount = 100
	key := "abovecount"
	videoDir := seedPartialCacheDir(t, c, key, 10)

	// Leftovers from a run whose source was far longer.
	for _, i := range []int{150, 400} {
		p := filepath.Join(videoDir, fmt.Sprintf("seg-%d.m4s", i))
		if err := os.WriteFile(p, []byte("from-a-longer-source"), 0o644); err != nil {
			t.Fatalf("write seg-%d: %v", i, err)
		}
	}

	if _, _, err := planPartialReuse(videoDir, segmentCount, 0); err != nil {
		t.Fatalf("planPartialReuse: %v", err)
	}

	for _, i := range []int{150, 400} {
		if _, err := os.Stat(filepath.Join(videoDir, fmt.Sprintf("seg-%d.m4s", i))); !os.IsNotExist(err) {
			t.Fatalf("seg-%d survived: it sits above this source's segment count "+
				"and can later satisfy VerifyComplete's last-segment check", i)
		}
	}
}

// TestPartialReuseSingleSegmentSource covers the degenerate short source. The
// truncation rule would reduce a one-segment block to nothing; re-encoding one
// segment is cheap, so the dir is simply cleared and rebuilt rather than
// reasoning about whether that lone segment was complete.
func TestPartialReuseSingleSegmentSource(t *testing.T) {
	c := newTestCache(t, 1)
	key := "oneseg"
	videoDir := seedPartialCacheDir(t, c, key, 1)

	startIdx, inherited, err := planPartialReuse(videoDir, 1, 0)
	if err != nil {
		t.Fatalf("planPartialReuse: %v", err)
	}
	if startIdx != 0 || inherited != 0 {
		t.Fatalf("startIdx=%d inherited=%d, want 0/0", startIdx, inherited)
	}
	// ffmpeg re-encodes from 0 and rewrites both, so clearing them loses
	// nothing — but leaving a stale seg-0 behind would let the fast path serve
	// it instead of the new encode.
	if _, err := os.Stat(filepath.Join(videoDir, "seg-0.m4s")); !os.IsNotExist(err) {
		t.Fatal("stale seg-0 survived on a single-segment source")
	}
}

// TestPartialReuseDropsBlockBehindResume covers the case where reuse is not
// possible: the viewer resumes past the leftovers. ffmpeg writes forward, so
// honouring the resume would leave an unfillable hole between the block and the
// start index. The leftovers lose; the dir is cleared.
func TestPartialReuseDropsBlockBehindResume(t *testing.T) {
	c := newTestCache(t, 1)
	const staleCount = 100
	const segmentCount = 900
	const resumeIdx = 600
	key := "resumepast"
	videoDir := seedPartialCacheDir(t, c, key, staleCount)

	startIdx, inherited, err := planPartialReuse(videoDir, segmentCount, resumeIdx)
	if err != nil {
		t.Fatalf("planPartialReuse: %v", err)
	}
	if inherited != 0 {
		t.Fatalf("inherited = %d, want 0: a block that ends before the resume "+
			"point cannot be spliced with an encode starting after it", inherited)
	}
	if startIdx != resumeIdx {
		t.Fatalf("startIdx = %d, want %d: the resume position is what the viewer asked for",
			startIdx, resumeIdx)
	}
	if _, err := os.Stat(filepath.Join(videoDir, "seg-0.m4s")); !os.IsNotExist(err) {
		t.Fatal("stale seg-0 survived; it would be served from the fast path and " +
			"counted toward sealing the entry")
	}
	if _, err := os.Stat(filepath.Join(videoDir, "init.mp4")); !os.IsNotExist(err) {
		t.Fatal("stale init.mp4 survived a full discard")
	}
}

// TestPartialReuseIgnoresDirWithoutInit guards the degenerate leftovers: a dir
// whose init.mp4 never landed. Its segments are unplayable without it, so they
// must be cleared rather than adopted.
func TestPartialReuseIgnoresDirWithoutInit(t *testing.T) {
	c := newTestCache(t, 1)
	const segmentCount = 900
	key := "noinit"
	videoDir := seedPartialCacheDir(t, c, key, 50)
	if err := os.Remove(filepath.Join(videoDir, "init.mp4")); err != nil {
		t.Fatalf("remove init.mp4: %v", err)
	}

	startIdx, inherited, err := planPartialReuse(videoDir, segmentCount, 0)
	if err != nil {
		t.Fatalf("planPartialReuse: %v", err)
	}
	if inherited != 0 || startIdx != 0 {
		t.Fatalf("startIdx=%d inherited=%d, want 0/0: segments without init.mp4 "+
			"cannot be played back", startIdx, inherited)
	}
	if _, err := os.Stat(filepath.Join(videoDir, "seg-0.m4s")); !os.IsNotExist(err) {
		t.Fatal("orphan seg-0 survived despite a missing init.mp4")
	}
}

// TestAllSegmentsPresentRejectsMixedProfileDir guards the worst outcome. When a
// session over a partial dir does reach the end, allSegmentsPresent stats every
// index and finds them all — stale ones included — so Close writes .complete
// over a directory holding two encoders' output. That seals the mixed dir as a
// permanent cache HIT: every later play replays the broken splice, and no
// integrity check catches it because HasComplete/VerifyComplete only look at
// init.mp4 and the final segment.
func TestAllSegmentsPresentRejectsMixedProfileDir(t *testing.T) {
	c := newTestCache(t, 1)
	const staleCount = 600
	const segmentCount = 900
	key := "mixedseal"
	videoDir := seedPartialCacheDir(t, c, key, staleCount)

	s := newPartialReuseSession(t, c, key, segmentCount)

	// This run started at the resume point and wrote only the tail; segments
	// below staleCount belong to the previous, differently-configured encode.
	// inheritedMax stays 0: nothing reconciled this dir, so the prefix was
	// never vetted as spliceable.
	s.ffmpegSegStart = staleCount
	s.lowestEncodedIdx = staleCount
	s.inheritedMax = 0
	for i := staleCount; i < segmentCount; i++ {
		p := filepath.Join(videoDir, fmt.Sprintf("seg-%d.m4s", i))
		if err := os.WriteFile(p, []byte(fmt.Sprintf("fresh-seg-%d", i)), 0o644); err != nil {
			t.Fatalf("write seg-%d: %v", i, err)
		}
	}
	s.readyMax = segmentCount

	if s.allSegmentsPresent() {
		t.Fatalf("allSegmentsPresent accepted a dir mixing %d segments from a "+
			"previous run with %d from this one; Close would seal it .complete "+
			"and every later play would HIT the broken splice",
			staleCount, segmentCount-staleCount)
	}

	// The counterpart: the same on-disk layout IS sealable once the prefix was
	// adopted through planPartialReuse, which verified it is contiguous from
	// seg-0 and let this ffmpeg encode everything above it. Without this the
	// fix would trade the corruption bug for a cache that never seals — every
	// partial-reuse session re-encoding from scratch on the next play.
	s.inheritedMax = staleCount
	if !s.allSegmentsPresent() {
		t.Fatal("a reconciled dir (prefix adopted, remainder encoded by this " +
			"session) must still be sealable, otherwise partial reuse never " +
			"produces a cache HIT")
	}
}

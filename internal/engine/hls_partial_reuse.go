package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Partial cache reuse.
//
// A cache dir without .complete holds the leftovers of a run that never
// finished. hls_cache.go promises the next session "can reuse the segments
// already present, ffmpeg fills the gaps" — this file is the part that makes
// that true. Without it the reuse is silently broken: pollSegments counts the
// dead run's files as the live encoder's output, so nothing ever writes the
// gap, and playback stops where the leftovers end.
//
// The rule everything here follows: a segment written by a PREVIOUS run is
// usable output but not evidence about the CURRENT encoder's progress. The
// session records how far the inherited block reaches (inheritedMax) and
// starts ffmpeg at the first hole, so the encoder only produces what is
// genuinely missing.
//
// Why a session may only inherit a prefix starting at seg-0: segment N's
// bytes are only interchangeable with a fresh encode's segment N when every
// segment before it came from the same encode. A block starting mid-file
// (left by a resumed run) is dropped rather than spliced — see
// scanInheritedSegments.

// scanInheritedSegments counts the contiguous run of usable segments already
// present in dir, starting at seg-0, and reports whether init.mp4 is there.
//
// Contiguity from 0 is required, not incidental: the count becomes the index
// where ffmpeg starts, and ffmpeg writes forward from there. A gap below that
// index would never be filled by anyone.
//
// The final segment of the inherited block is deliberately excluded. A run
// that was killed mid-write leaves its last file truncated, and truncated
// bytes are indistinguishable from complete ones by stat alone — the same
// reason pollSegments only trusts a segment once its successor exists. Giving
// the last one back to ffmpeg costs one segment of re-encoding and removes the
// guesswork.
func scanInheritedSegments(videoDir string, segmentCount int) (count int, hasInit bool) {
	if fi, err := os.Stat(filepath.Join(videoDir, "init.mp4")); err != nil || fi.Size() == 0 {
		return 0, false
	}
	present := 0
	for i := 0; i < segmentCount; i++ {
		fi, err := os.Stat(filepath.Join(videoDir, fmt.Sprintf("seg-%d.m4s", i)))
		if err != nil || fi.Size() == 0 {
			break
		}
		present++
	}
	if present == 0 {
		return 0, true
	}
	// Drop the possibly-truncated tail. A complete block (present ==
	// segmentCount) has no truncated tail — its last segment was sealed by an
	// ffmpeg that ran to the end — but such a dir would carry .complete and be
	// served as a HIT long before reaching this path, so treating it the same
	// way costs nothing and keeps the rule single-branched.
	//
	// The exception is a source short enough to be one segment: there the
	// dropped tail IS the whole block, and returning 0 would have the caller
	// wipe a dir that may hold a perfectly good encode. Re-encoding a single
	// segment is cheap, so hand it back to ffmpeg rather than reason about
	// whether it was truncated.
	if present == 1 {
		return 0, true
	}
	return present - 1, true
}

// dropInheritedSegments removes every seg-*.m4s at or above `from`, plus
// init.mp4 when nothing is being kept.
//
// Called when the inherited block cannot be spliced with what this session is
// about to encode. The files must go rather than simply be ignored: both
// ServeSegment's on-disk fast path and allSegmentsPresent read the directory
// directly, so a stale file left in place would still be served to a viewer
// and still count toward sealing the entry.
//
// The directory is enumerated rather than walked over 0..segmentCount, because
// leftovers are not bounded by the CURRENT probe's segment count. A previous
// run of a longer source — or one that used a different segment duration —
// leaves indices above it, and those survive a bounded loop. VerifyComplete
// stats the last expected segment, so a stray high-index file is exactly the
// kind of evidence that later validates a directory it should have condemned.
func dropInheritedSegments(videoDir string, from int) error {
	if from <= 0 {
		if err := os.Remove(filepath.Join(videoDir, "init.mp4")); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("hls: drop init.mp4: %w", err)
		}
	}
	names, err := segmentNamesFrom(videoDir, from)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := os.Remove(filepath.Join(videoDir, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("hls: drop %s: %w", name, err)
		}
	}
	return nil
}

// segmentNamesFrom lists the segment files in videoDir whose index is `from`
// or higher. A missing directory yields nothing rather than an error — the
// caller's intent is "leave no segment at or above this index", which an
// absent dir already satisfies.
func segmentNamesFrom(videoDir string, from int) ([]string, error) {
	entries, err := os.ReadDir(videoDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("hls: read video dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if idx, ok := segmentIndexFromName(e.Name()); ok && idx >= from {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// segmentIndexFromName parses "seg-<n>.m4s" into n. Anything else — init.mp4,
// a copy-mode .ts segment, a temp file ffmpeg has not renamed yet — reports
// false and is left alone.
func segmentIndexFromName(name string) (int, bool) {
	const prefix, suffix = "seg-", ".m4s"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return 0, false
	}
	n, err := strconv.Atoi(name[len(prefix) : len(name)-len(suffix)])
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// planPartialReuse decides where ffmpeg should start in a cache dir that may
// hold leftovers, and leaves the directory consistent with that decision.
//
// Returns the segment index ffmpeg must start at, and how many leading
// segments the session inherited. The two are equal on the reuse path; both
// are 0 when the leftovers were discarded.
//
// wantStart is the index the session would use with an empty dir — 0, or the
// resume point. Reuse is only taken when the inherited block reaches it,
// because ffmpeg writes forward: starting at a resume point past the block
// would strand an unfillable hole between them, which is exactly the stall
// this whole file exists to prevent. When the block falls short, the resume
// wins and the leftovers are discarded.
func planPartialReuse(videoDir string, segmentCount, wantStart int) (startIdx, inherited int, err error) {
	inherited, hasInit := scanInheritedSegments(videoDir, segmentCount)
	if !hasInit || inherited <= 0 {
		// Nothing usable. Clear whatever is there so no orphan file can be
		// served later, and encode from wantStart as if the dir were new.
		if err := dropInheritedSegments(videoDir, 0); err != nil {
			return 0, 0, err
		}
		return wantStart, 0, nil
	}
	if inherited < wantStart {
		// The viewer resumes past the inherited block. Keeping it would leave
		// a permanent gap; the resume position is what the viewer asked for,
		// so the block loses.
		if err := dropInheritedSegments(videoDir, 0); err != nil {
			return 0, 0, err
		}
		return wantStart, 0, nil
	}
	// Reuse: keep seg-0..seg-(inherited-1) and encode the remainder. Anything
	// at or above the start index is discarded — it may come from a different
	// encoder configuration, and ffmpeg is about to overwrite those indices
	// anyway.
	if err := dropInheritedSegments(videoDir, inherited); err != nil {
		return 0, 0, err
	}
	return inherited, inherited, nil
}

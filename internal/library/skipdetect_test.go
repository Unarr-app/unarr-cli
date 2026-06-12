package library

import (
	"testing"

	"github.com/torrentclaw/unarr/internal/library/mediainfo"
)

func TestCreditsFromBlackRuns_DetectsCreditsRoll(t *testing.T) {
	// Movie of 7200s; credits-on-black from 6900 to the end, black frame every
	// ~2s, plus a stray fade-to-black at 5000s that must not win.
	var times []float64
	times = append(times, 5000, 5001) // short mid-film fade
	for tt := 6900.0; tt <= 7195; tt += 2 {
		times = append(times, tt)
	}
	segs := creditsFromBlackRuns(times, 7200)
	if len(segs) != 1 {
		t.Fatalf("expected 1 credits segment, got %d", len(segs))
	}
	s := segs[0]
	if s.Category != "credits" {
		t.Errorf("category = %q, want credits", s.Category)
	}
	if s.StartSec < 6890 || s.StartSec > 6910 {
		t.Errorf("StartSec = %.1f, want ≈ 6900", s.StartSec)
	}
	if s.EndSec != 7200 {
		t.Errorf("EndSec = %.1f, want 7200 (file end)", s.EndSec)
	}
}

func TestCreditsFromBlackRuns_RejectsShortFade(t *testing.T) {
	// Only a 20s black run near the end — too short to be credits.
	var times []float64
	for tt := 7170.0; tt <= 7190; tt += 2 {
		times = append(times, tt)
	}
	if segs := creditsFromBlackRuns(times, 7200); len(segs) != 0 {
		t.Fatalf("expected no segments for a 20s fade, got %+v", segs)
	}
}

func TestCreditsFromBlackRuns_RejectsRunNotReachingEnd(t *testing.T) {
	// 120s black run that ends 300s before the file end (a long mid-film
	// montage on black) — must not be flagged as credits.
	var times []float64
	for tt := 6700.0; tt <= 6820; tt += 2 {
		times = append(times, tt)
	}
	if segs := creditsFromBlackRuns(times, 7200); len(segs) != 0 {
		t.Fatalf("expected no segments when run stops mid-film, got %+v", segs)
	}
}

func TestDetectForEpisode_PrefersDifferentEpisodePartners(t *testing.T) {
	// Sanity: an episode with no partners (all same episode number) yields nil.
	it := LibraryItem{FilePath: "/a/e1.mkv", Episode: 1, Season: 1}
	dup := LibraryItem{FilePath: "/a/e1-other-release.mkv", Episode: 1, Season: 1}
	fps := map[string]*episodeFingerprints{
		it.FilePath:  {duration: 1400, intro: []uint32{1, 2, 3}, credits: []uint32{4, 5, 6}},
		dup.FilePath: {duration: 1400, intro: []uint32{1, 2, 3}, credits: []uint32{4, 5, 6}},
	}
	segs := detectForEpisode(it, fps[it.FilePath], []LibraryItem{it, dup}, fps)
	if len(segs) != 0 {
		t.Fatalf("expected no segments without a different-episode partner, got %+v", segs)
	}
}

var _ = mediainfo.SkipSegmentRange{} // keep import for future use

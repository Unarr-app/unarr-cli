package mediainfo

import (
	"math"
	"testing"
)

// lcg is a tiny deterministic pseudo-random stream for synthetic fingerprints.
type lcg struct{ state uint64 }

func (l *lcg) next() uint32 {
	l.state = l.state*6364136223846793005 + 1442695040888963407
	return uint32(l.state >> 32)
}

func TestFindSharedRegion_DetectsAlignedSegment(t *testing.T) {
	// Shared segment: 700 points ≈ 86.7s — a typical anime OP.
	shared := make([]uint32, 700)
	g := &lcg{state: 42}
	for i := range shared {
		shared[i] = g.next()
	}

	// a: 80 points of unique noise, then the shared segment, then noise.
	ga := &lcg{state: 1001}
	a := make([]uint32, 0, 2000)
	for i := 0; i < 80; i++ {
		a = append(a, ga.next())
	}
	a = append(a, shared...)
	for len(a) < 2000 {
		a = append(a, ga.next())
	}

	// b: 480 points of different noise, then the same shared segment.
	gb := &lcg{state: 2002}
	b := make([]uint32, 0, 2000)
	for i := 0; i < 480; i++ {
		b = append(b, gb.next())
	}
	b = append(b, shared...)
	for len(b) < 2000 {
		b = append(b, gb.next())
	}

	r := FindSharedRegion(a, b, 15, 120)
	if r == nil {
		t.Fatal("expected a shared region, got nil")
	}
	wantAStart := 80 * ChromaprintSampleDur
	wantBStart := 480 * ChromaprintSampleDur
	if math.Abs(r.AStart-wantAStart) > 2 {
		t.Errorf("AStart = %.1f, want ≈ %.1f", r.AStart, wantAStart)
	}
	if math.Abs(r.BStart-wantBStart) > 2 {
		t.Errorf("BStart = %.1f, want ≈ %.1f", r.BStart, wantBStart)
	}
	wantDur := 700 * ChromaprintSampleDur
	if math.Abs(r.Duration-wantDur) > 4 {
		t.Errorf("Duration = %.1f, want ≈ %.1f", r.Duration, wantDur)
	}
}

func TestFindSharedRegion_NoMatchOnNoise(t *testing.T) {
	ga, gb := &lcg{state: 7}, &lcg{state: 9}
	a := make([]uint32, 1500)
	b := make([]uint32, 1500)
	for i := range a {
		a[i] = ga.next()
		b[i] = gb.next()
	}
	if r := FindSharedRegion(a, b, 15, 120); r != nil {
		t.Fatalf("expected nil on unrelated noise, got %+v", r)
	}
}

func TestFindSharedRegion_FullMatchExceedsMaxDur(t *testing.T) {
	// Two identical streams (same episode, two releases): the only region is
	// the full window, which must be rejected by maxDur.
	g := &lcg{state: 5}
	a := make([]uint32, 2000)
	for i := range a {
		a[i] = g.next()
	}
	b := make([]uint32, 2000)
	copy(b, a)
	if r := FindSharedRegion(a, b, 15, 120); r != nil {
		t.Fatalf("expected nil for identical streams (region > maxDur), got %+v", r)
	}
}

func TestFindSharedRegion_ToleratesBitNoise(t *testing.T) {
	// Same shared segment but with ≤2 flipped bits per point (re-encode noise).
	shared := make([]uint32, 600)
	g := &lcg{state: 77}
	for i := range shared {
		shared[i] = g.next()
	}
	noisy := make([]uint32, len(shared))
	for i, v := range shared {
		noisy[i] = v ^ (1 << uint(i%20)) // flip one bit
	}

	ga, gb := &lcg{state: 100}, &lcg{state: 200}
	a := append(make([]uint32, 0, 1500), shared...)
	for len(a) < 1500 {
		a = append(a, ga.next())
	}
	b := append(make([]uint32, 0, 1500), noisy...)
	for len(b) < 1500 {
		b = append(b, gb.next())
	}

	r := FindSharedRegion(a, b, 15, 120)
	if r == nil {
		t.Fatal("expected match despite 1-bit noise, got nil")
	}
	if r.AStart > 2 {
		t.Errorf("AStart = %.1f, want ≈ 0", r.AStart)
	}
}

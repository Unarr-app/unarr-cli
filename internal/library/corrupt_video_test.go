package library

import (
	"os"
	"path/filepath"
	"testing"
)

// writeZeroHead writes a video-sized file whose first fpChunk bytes are all NUL but
// whose tail is non-zero — a "zero-content" (unplayable) download of the RIGHT size.
func writeZeroHead(t *testing.T, path string, totalSize int) {
	t.Helper()
	// head = 0 (default), mid = 0, tail = non-zero. writeHeadMidTail sets head/tail
	// explicitly; head 0x00 keeps the first MiB zeroed.
	writeHeadMidTail(t, path, totalSize, 0x00, 0xAB, 0xCD)
}

// realVideoOpts returns the default options plus the corrupt-video toggle ON.
func corruptOn() ReconcileOptions {
	o := DefaultReconcileOptions()
	o.RemoveCorruptVideos = true
	return o
}

// --- FirstOrLastMiBAllZero (helper) -------------------------------------------

func TestFirstOrLastMiBAllZero(t *testing.T) {
	root := t.TempDir()

	zeroHead := filepath.Join(root, "zerohead.mkv")
	writeHeadMidTail(t, zeroHead, 3*fpChunk, 0x00, 0xAB, 0xCD) // first MiB all zero
	if z, err := FirstOrLastMiBAllZero(zeroHead, 3*fpChunk); err != nil || !z {
		t.Errorf("zero-head: got (%v,%v), want (true,nil)", z, err)
	}

	zeroTail := filepath.Join(root, "zerotail.mkv")
	writeHeadMidTail(t, zeroTail, 3*fpChunk, 0xAB, 0xCD, 0x00) // last MiB all zero
	if z, err := FirstOrLastMiBAllZero(zeroTail, 3*fpChunk); err != nil || !z {
		t.Errorf("zero-tail: got (%v,%v), want (true,nil)", z, err)
	}

	real := filepath.Join(root, "real.mkv")
	writeHeadMidTail(t, real, 3*fpChunk, 0xAB, 0xCD, 0xEF) // non-zero extremes
	if z, err := FirstOrLastMiBAllZero(real, 3*fpChunk); err != nil || z {
		t.Errorf("real video: got (%v,%v), want (false,nil)", z, err)
	}

	// Small file (<= 2*fpChunk) checked whole: all-zero small → true, non-zero → false.
	smallZero := filepath.Join(root, "smallzero.mkv")
	writeSized(t, smallZero, 4096) // writeSized fills with zeros
	if z, err := FirstOrLastMiBAllZero(smallZero, 4096); err != nil || !z {
		t.Errorf("small all-zero: got (%v,%v), want (true,nil)", z, err)
	}
	smallReal := filepath.Join(root, "smallreal.mkv")
	writeVideoWithMarker(t, smallReal, 4096, 0xAB)
	if z, err := FirstOrLastMiBAllZero(smallReal, 4096); err != nil || z {
		t.Errorf("small non-zero: got (%v,%v), want (false,nil)", z, err)
	}
}

// --- classify: toggle gating --------------------------------------------------

// TestReconcileCorruptVideoToggle: a right-sized zero-head video is flagged
// KindCorruptVideo ONLY with the toggle on; off → no finding.
func TestReconcileCorruptVideoToggle(t *testing.T) {
	for _, on := range []bool{false, true} {
		name := "off"
		if on {
			name = "on"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			corrupt := filepath.Join(root, "S01E03 (2).mkv")
			writeZeroHead(t, corrupt, 3*fpChunk) // >= 1 MiB floor, zero head

			opts := DefaultReconcileOptions()
			opts.RemoveCorruptVideos = on
			findings, err := Reconcile(ReconcilePaths{DownloadDir: root}, nil, opts)
			if err != nil {
				t.Fatal(err)
			}
			got := hasKind(findings, KindCorruptVideo)
			if got != on {
				t.Errorf("RemoveCorruptVideos=%v: hasCorrupt=%v, want %v (findings=%+v)", on, got, on, findings)
			}
			// Dry-run: never removed.
			mustExist(t, corrupt)
		})
	}
}

// TestReconcileCorruptVideoApplyRemoves: with the toggle on and --apply, the
// corrupt video is removed.
func TestReconcileCorruptVideoApplyRemoves(t *testing.T) {
	root := t.TempDir()
	corrupt := filepath.Join(root, "corrupt.mkv")
	writeZeroHead(t, corrupt, 3*fpChunk)

	opts := corruptOn()
	opts.Apply = true
	if _, err := Reconcile(ReconcilePaths{DownloadDir: root}, nil, opts); err != nil {
		t.Fatal(err)
	}
	mustGone(t, corrupt)
}

// --- classify: precedence / non-flagging --------------------------------------

// TestReconcileCorruptVsStubVsReal: a stub (< floor) stays a stub even with the
// toggle on; a real video (>= floor, non-zero content) is never flagged.
func TestReconcileCorruptVsStubVsReal(t *testing.T) {
	root := t.TempDir()

	// Stub: below floor, and zero content — must classify as STUB, not corrupt
	// (stub check runs first).
	stub := filepath.Join(root, "stub.mkv")
	writeSized(t, stub, 512) // < 1 MiB, all zeros

	// Real: >= floor, non-zero extremes → not corrupt, not flagged at all.
	real := filepath.Join(root, "real.mkv")
	writeHeadMidTail(t, real, 3*fpChunk, 0xAB, 0xCD, 0xEF)

	// Corrupt: >= floor, zero head.
	corrupt := filepath.Join(root, "corrupt.mkv")
	writeZeroHead(t, corrupt, 3*fpChunk)

	findings, err := Reconcile(ReconcilePaths{DownloadDir: root}, nil, corruptOn())
	if err != nil {
		t.Fatal(err)
	}

	kindOf := map[string]FindingKind{}
	for _, f := range findings {
		kindOf[filepath.Base(f.Path)] = f.Kind
	}
	if kindOf["stub.mkv"] != KindStubVideo {
		t.Errorf("stub.mkv kind = %q, want stub_video (stub check must win over corrupt)", kindOf["stub.mkv"])
	}
	if _, flagged := kindOf["real.mkv"]; flagged {
		t.Errorf("real.mkv must NOT be flagged, got %q", kindOf["real.mkv"])
	}
	if kindOf["corrupt.mkv"] != KindCorruptVideo {
		t.Errorf("corrupt.mkv kind = %q, want corrupt_video", kindOf["corrupt.mkv"])
	}
}

// --- safeKinds exclusion ------------------------------------------------------

// TestCorruptVideoNotSafe asserts KindCorruptVideo is NOT auto-removable: it is
// absent from safeKinds and IsSafe() is false, so the daemon's ReconcileSafe never
// removes it even with the toggle on.
func TestCorruptVideoNotSafe(t *testing.T) {
	if (Finding{Kind: KindCorruptVideo}).IsSafe() {
		t.Error("KindCorruptVideo must NOT be safe (auto-sweep must never remove it)")
	}
	// Every OTHER emitted kind stays safe (sanity that we didn't break the set).
	for _, k := range []FindingKind{KindStubVideo, KindOrphanPartial, KindOrphanSidecar, KindEmptyDir, KindMediaNamedDir, KindDuplicate} {
		if !(Finding{Kind: k}).IsSafe() {
			t.Errorf("kind %s should be safe", k)
		}
	}
}

// TestReconcileSafeSkipsCorruptVideo: even with the toggle on, the daemon's
// ReconcileSafe (safe-subset apply) does NOT remove a corrupt video.
func TestReconcileSafeSkipsCorruptVideo(t *testing.T) {
	root := t.TempDir()
	corrupt := filepath.Join(root, "corrupt.mkv")
	writeZeroHead(t, corrupt, 3*fpChunk)

	safe, _, _, err := ReconcileSafe(ReconcilePaths{DownloadDir: root}, nil, corruptOn())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range safe {
		if f.Kind == KindCorruptVideo {
			t.Error("ReconcileSafe must never apply a corrupt_video finding")
		}
	}
	if _, err := os.Stat(corrupt); err != nil {
		t.Errorf("auto-sweep (ReconcileSafe) must leave the corrupt video on disk: %v", err)
	}
}

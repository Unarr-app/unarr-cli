package download

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"time"

	"github.com/torrentclaw/torrentclaw-cli/internal/usenet/nzb"
)

var fixedPast = time.Now().Add(-30 * 24 * time.Hour)

func makeTestNZB(fileCount, segsPerFile int) *nzb.NZB {
	n := &nzb.NZB{
		Files: make([]nzb.File, fileCount),
	}
	for i := 0; i < fileCount; i++ {
		segs := make([]nzb.Segment, segsPerFile)
		for j := 0; j < segsPerFile; j++ {
			segs[j] = nzb.Segment{
				Bytes:     750 * 1024,
				Number:    j + 1,
				MessageID: segMsgID(i, j),
			}
		}
		n.Files[i] = nzb.File{
			Subject:  `"testfile_` + string(rune('a'+i)) + `.rar" yEnc (1/` + string(rune('0'+segsPerFile)) + `)`,
			Segments: segs,
		}
	}
	return n
}

func segMsgID(file, seg int) string {
	return "part" + itoa(seg) + ".file" + itoa(file) + "@example.com"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

func TestFingerprint_Deterministic(t *testing.T) {
	n := makeTestNZB(3, 10)
	fp1 := Fingerprint(n)
	fp2 := Fingerprint(n)
	if fp1 != fp2 {
		t.Fatal("fingerprint should be deterministic")
	}
}

func TestFingerprint_DifferentNZB(t *testing.T) {
	n1 := makeTestNZB(3, 10)
	n2 := makeTestNZB(3, 11)
	if Fingerprint(n1) == Fingerprint(n2) {
		t.Fatal("different NZBs should have different fingerprints")
	}
}

func TestProgressTracker_NewAndFlush(t *testing.T) {
	dir := t.TempDir()
	n := makeTestNZB(2, 5)
	tracker := NewProgressTracker("test-task-1", n, dir)

	// Mark some segments
	tracker.MarkDone(0, 0)
	tracker.MarkDone(0, 2)
	tracker.MarkDone(1, 4)

	if err := tracker.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// Verify file exists
	path := filepath.Join(dir, "test-task-1.progress")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("progress file should exist: %v", err)
	}

	// Verify state
	if !tracker.IsDone(0, 0) {
		t.Error("segment 0,0 should be done")
	}
	if tracker.IsDone(0, 1) {
		t.Error("segment 0,1 should NOT be done")
	}
	if !tracker.IsDone(0, 2) {
		t.Error("segment 0,2 should be done")
	}
	if !tracker.IsDone(1, 4) {
		t.Error("segment 1,4 should be done")
	}
	if tracker.CompletedSegments(0) != 2 {
		t.Errorf("file 0: expected 2 completed, got %d", tracker.CompletedSegments(0))
	}
	if tracker.CompletedSegments(1) != 1 {
		t.Errorf("file 1: expected 1 completed, got %d", tracker.CompletedSegments(1))
	}
}

func TestProgressTracker_LoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	n := makeTestNZB(2, 8)

	// Create and populate
	tracker1 := NewProgressTracker("test-task-2", n, dir)
	tracker1.MarkDone(0, 0)
	tracker1.MarkDone(0, 3)
	tracker1.MarkDone(0, 7)
	tracker1.MarkDone(1, 1)
	tracker1.MarkDone(1, 5)
	if err := tracker1.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// Load into new tracker
	tracker2 := NewProgressTracker("test-task-2", n, dir)
	loaded, err := tracker2.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !loaded {
		t.Fatal("should have loaded successfully")
	}

	// Verify all bits match
	for _, tc := range []struct {
		file, seg int
		want      bool
	}{
		{0, 0, true}, {0, 1, false}, {0, 2, false}, {0, 3, true},
		{0, 4, false}, {0, 5, false}, {0, 6, false}, {0, 7, true},
		{1, 0, false}, {1, 1, true}, {1, 2, false}, {1, 3, false},
		{1, 4, false}, {1, 5, true}, {1, 6, false}, {1, 7, false},
	} {
		got := tracker2.IsDone(tc.file, tc.seg)
		if got != tc.want {
			t.Errorf("file %d seg %d: got %v, want %v", tc.file, tc.seg, got, tc.want)
		}
	}

	if tracker2.CompletedSegments(0) != 3 {
		t.Errorf("file 0: expected 3 completed, got %d", tracker2.CompletedSegments(0))
	}
	if tracker2.CompletedSegments(1) != 2 {
		t.Errorf("file 1: expected 2 completed, got %d", tracker2.CompletedSegments(1))
	}
}

func TestProgressTracker_FingerprintMismatch(t *testing.T) {
	dir := t.TempDir()
	n1 := makeTestNZB(2, 5)
	n2 := makeTestNZB(2, 6) // different segment count = different fingerprint

	// Write with n1
	tracker1 := NewProgressTracker("test-task-3", n1, dir)
	tracker1.MarkDone(0, 0)
	if err := tracker1.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// Try to load with n2
	tracker2 := NewProgressTracker("test-task-3", n2, dir)
	loaded, err := tracker2.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded {
		t.Fatal("should NOT load — fingerprint mismatch")
	}
}

func TestProgressTracker_IsFileDone(t *testing.T) {
	dir := t.TempDir()
	n := makeTestNZB(1, 4)
	tracker := NewProgressTracker("test-task-4", n, dir)

	if tracker.IsFileDone(0) {
		t.Error("file should not be done yet")
	}

	tracker.MarkDone(0, 0)
	tracker.MarkDone(0, 1)
	tracker.MarkDone(0, 2)
	if tracker.IsFileDone(0) {
		t.Error("file should not be done (3/4)")
	}

	tracker.MarkDone(0, 3)
	if !tracker.IsFileDone(0) {
		t.Error("file should be done (4/4)")
	}
}

func TestProgressTracker_ConcurrentMark(t *testing.T) {
	dir := t.TempDir()
	segCount := 1000
	n := makeTestNZB(1, segCount)
	tracker := NewProgressTracker("test-task-5", n, dir)

	var wg sync.WaitGroup
	for i := 0; i < segCount; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			tracker.MarkDone(0, idx)
		}(i)
	}
	wg.Wait()

	if !tracker.IsFileDone(0) {
		t.Errorf("all segments should be done, got %d/%d", tracker.CompletedSegments(0), segCount)
	}

	// Flush and reload
	if err := tracker.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	tracker2 := NewProgressTracker("test-task-5", n, dir)
	loaded, _ := tracker2.Load()
	if !loaded {
		t.Fatal("should load")
	}
	if !tracker2.IsFileDone(0) {
		t.Errorf("after reload: expected all done, got %d/%d", tracker2.CompletedSegments(0), segCount)
	}
}

func TestProgressTracker_Remove(t *testing.T) {
	dir := t.TempDir()
	n := makeTestNZB(1, 3)
	tracker := NewProgressTracker("test-task-6", n, dir)
	tracker.MarkDone(0, 0)
	if err := tracker.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// Write a fake NZB cache file
	nzbPath := filepath.Join(dir, "test-task-6.nzb")
	os.WriteFile(nzbPath, []byte("<nzb/>"), 0o644)

	// Both should exist
	if _, err := os.Stat(tracker.progressPath()); err != nil {
		t.Fatal("progress file should exist")
	}
	if _, err := os.Stat(nzbPath); err != nil {
		t.Fatal("nzb cache should exist")
	}

	tracker.Remove()

	if _, err := os.Stat(tracker.progressPath()); !os.IsNotExist(err) {
		t.Error("progress file should be removed")
	}
	if _, err := os.Stat(nzbPath); !os.IsNotExist(err) {
		t.Error("nzb cache should be removed")
	}
}

func TestProgressTracker_LargeNZB(t *testing.T) {
	dir := t.TempDir()
	segCount := 30000
	n := makeTestNZB(1, segCount)
	tracker := NewProgressTracker("test-task-7", n, dir)

	// Mark every other segment
	for i := 0; i < segCount; i += 2 {
		tracker.MarkDone(0, i)
	}

	if err := tracker.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// Check file size is compact
	info, err := os.Stat(tracker.progressPath())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Header (40) + file header (4) + bitset (30000/8 = 3750) = 3794 bytes
	expectedMax := int64(4000)
	if info.Size() > expectedMax {
		t.Errorf("progress file too large: %d bytes (expected < %d)", info.Size(), expectedMax)
	}

	// Reload and verify
	tracker2 := NewProgressTracker("test-task-7", n, dir)
	loaded, _ := tracker2.Load()
	if !loaded {
		t.Fatal("should load")
	}
	if tracker2.CompletedSegments(0) != segCount/2 {
		t.Errorf("expected %d completed, got %d", segCount/2, tracker2.CompletedSegments(0))
	}
	// Spot check
	if !tracker2.IsDone(0, 0) {
		t.Error("seg 0 should be done")
	}
	if tracker2.IsDone(0, 1) {
		t.Error("seg 1 should NOT be done")
	}
	if !tracker2.IsDone(0, 100) {
		t.Error("seg 100 should be done")
	}
}

func TestProgressTracker_CompletedBytes(t *testing.T) {
	dir := t.TempDir()
	n := makeTestNZB(1, 4)
	tracker := NewProgressTracker("test-task-8", n, dir)

	tracker.MarkDone(0, 0)
	tracker.MarkDone(0, 2)

	bytes := tracker.CompletedBytes(0, n.Files[0].Segments)
	expected := int64(2 * 750 * 1024) // 2 segments * 750KB
	if bytes != expected {
		t.Errorf("expected %d bytes, got %d", expected, bytes)
	}
}

func TestProgressTracker_BoundsCheck(t *testing.T) {
	dir := t.TempDir()
	n := makeTestNZB(1, 3)
	tracker := NewProgressTracker("test-task-9", n, dir)

	// Out-of-bounds should not panic
	tracker.MarkDone(-1, 0)
	tracker.MarkDone(0, -1)
	tracker.MarkDone(5, 0)
	tracker.MarkDone(0, 100)

	if tracker.IsDone(-1, 0) {
		t.Error("out of bounds should return false")
	}
	if tracker.IsDone(5, 0) {
		t.Error("out of bounds should return false")
	}
	if tracker.IsFileDone(-1) {
		t.Error("out of bounds should return false")
	}
}

func TestCleanStaleFiles(t *testing.T) {
	dir := t.TempDir()

	// Create a "stale" file
	stalePath := filepath.Join(dir, "old-task.progress")
	os.WriteFile(stalePath, []byte("data"), 0o644)
	// Backdate modification time
	staleTime := os.Chtimes(stalePath, fixedPast, fixedPast)
	if staleTime != nil {
		t.Fatalf("chtimes: %v", staleTime)
	}

	// Create a "fresh" file
	freshPath := filepath.Join(dir, "new-task.progress")
	os.WriteFile(freshPath, []byte("data"), 0o644)

	removed := CleanStaleFiles(dir, 14*24*time.Hour) // 2 weeks — stale file is 30 days old
	if removed != 1 {
		t.Errorf("expected 1 removed, got %d", removed)
	}

	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Error("stale file should be removed")
	}
	if _, err := os.Stat(freshPath); err != nil {
		t.Error("fresh file should still exist")
	}
}

func TestProgressTracker_FlushNoOp(t *testing.T) {
	dir := t.TempDir()
	n := makeTestNZB(1, 3)
	tracker := NewProgressTracker("test-task-10", n, dir)

	// Flush without any marks should be no-op
	if err := tracker.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// File should not be created
	if _, err := os.Stat(tracker.progressPath()); !os.IsNotExist(err) {
		t.Error("no file should be created for empty flush")
	}
}

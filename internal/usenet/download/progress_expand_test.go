package download

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/torrentclaw/unarr/internal/usenet/nzb"
)

// --- Fingerprint ---

func TestFingerprint_EmptyNZB(t *testing.T) {
	n := &nzb.NZB{}
	fp := Fingerprint(n)
	// Empty NZB should still produce a deterministic hash (of zero message IDs).
	fp2 := Fingerprint(n)
	if fp != fp2 {
		t.Fatal("fingerprint of empty NZB should be deterministic")
	}
}

func TestFingerprint_OrderIndependent(t *testing.T) {
	// Fingerprint sorts IDs, so different file order should produce the same hash.
	n1 := &nzb.NZB{
		Files: []nzb.File{
			{Segments: []nzb.Segment{{MessageID: "a@x"}, {MessageID: "b@x"}}},
			{Segments: []nzb.Segment{{MessageID: "c@x"}}},
		},
	}
	n2 := &nzb.NZB{
		Files: []nzb.File{
			{Segments: []nzb.Segment{{MessageID: "c@x"}}},
			{Segments: []nzb.Segment{{MessageID: "b@x"}, {MessageID: "a@x"}}},
		},
	}
	if Fingerprint(n1) != Fingerprint(n2) {
		t.Fatal("fingerprint should be order-independent (sorted by message ID)")
	}
}

func TestFingerprint_SingleSegment(t *testing.T) {
	n := &nzb.NZB{
		Files: []nzb.File{
			{Segments: []nzb.Segment{{MessageID: "only@one"}}},
		},
	}
	fp := Fingerprint(n)
	if fp == [32]byte{} {
		t.Fatal("fingerprint should not be zero for a non-empty NZB")
	}
}

// --- ProgressTracker MarkDone idempotency ---

func TestProgressTracker_MarkDoneIdempotent(t *testing.T) {
	dir := t.TempDir()
	n := makeTestNZB(1, 5)
	tracker := NewProgressTracker("idem", n, dir)

	tracker.MarkDone(0, 2)
	if tracker.CompletedSegments(0) != 1 {
		t.Fatalf("expected 1, got %d", tracker.CompletedSegments(0))
	}

	// Mark the same segment again — count should not increase.
	tracker.MarkDone(0, 2)
	if tracker.CompletedSegments(0) != 1 {
		t.Fatalf("idempotent mark: expected 1, got %d", tracker.CompletedSegments(0))
	}
}

// --- ProgressTracker TotalCompleted ---

func TestProgressTracker_TotalCompleted(t *testing.T) {
	dir := t.TempDir()
	n := makeTestNZB(3, 4) // 3 files, 4 segs each
	tracker := NewProgressTracker("total", n, dir)

	tracker.MarkDone(0, 0)
	tracker.MarkDone(0, 1)
	tracker.MarkDone(1, 3)
	tracker.MarkDone(2, 0)
	tracker.MarkDone(2, 1)
	tracker.MarkDone(2, 2)

	if got := tracker.TotalCompleted(); got != 6 {
		t.Errorf("TotalCompleted: got %d, want 6", got)
	}
}

func TestProgressTracker_TotalCompleted_Empty(t *testing.T) {
	dir := t.TempDir()
	n := makeTestNZB(2, 3)
	tracker := NewProgressTracker("empty-total", n, dir)

	if got := tracker.TotalCompleted(); got != 0 {
		t.Errorf("TotalCompleted on fresh tracker: got %d, want 0", got)
	}
}

// --- CompletedBytes edge cases ---

func TestProgressTracker_CompletedBytes_OutOfBounds(t *testing.T) {
	dir := t.TempDir()
	n := makeTestNZB(1, 3)
	tracker := NewProgressTracker("cb-oob", n, dir)

	if got := tracker.CompletedBytes(-1, n.Files[0].Segments); got != 0 {
		t.Errorf("CompletedBytes with file -1: got %d, want 0", got)
	}
	if got := tracker.CompletedBytes(5, n.Files[0].Segments); got != 0 {
		t.Errorf("CompletedBytes with file 5: got %d, want 0", got)
	}
}

func TestProgressTracker_CompletedBytes_AllDone(t *testing.T) {
	dir := t.TempDir()
	n := makeTestNZB(1, 3)
	tracker := NewProgressTracker("cb-all", n, dir)

	for i := 0; i < 3; i++ {
		tracker.MarkDone(0, i)
	}

	got := tracker.CompletedBytes(0, n.Files[0].Segments)
	expected := int64(3 * 750 * 1024)
	if got != expected {
		t.Errorf("CompletedBytes all done: got %d, want %d", got, expected)
	}
}

// --- CompletedSegments out of bounds ---

func TestProgressTracker_CompletedSegments_OutOfBounds(t *testing.T) {
	dir := t.TempDir()
	n := makeTestNZB(1, 3)
	tracker := NewProgressTracker("cs-oob", n, dir)

	if got := tracker.CompletedSegments(-1); got != 0 {
		t.Errorf("CompletedSegments(-1) = %d, want 0", got)
	}
	if got := tracker.CompletedSegments(99); got != 0 {
		t.Errorf("CompletedSegments(99) = %d, want 0", got)
	}
}

// --- Load with corrupted / truncated data ---

func TestProgressTracker_Load_TruncatedHeader(t *testing.T) {
	dir := t.TempDir()
	n := makeTestNZB(1, 3)
	tracker := NewProgressTracker("trunc", n, dir)

	// Write too-short data
	os.WriteFile(tracker.progressPath(), []byte("UNR"), 0o644)

	loaded, err := tracker.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loaded {
		t.Error("truncated header should not load")
	}
}

func TestProgressTracker_Load_BadMagic(t *testing.T) {
	dir := t.TempDir()
	n := makeTestNZB(1, 3)
	tracker := NewProgressTracker("badmagic", n, dir)

	// Write data with wrong magic bytes
	data := make([]byte, headerSize+10)
	copy(data[0:4], []byte("BAAD"))
	os.WriteFile(tracker.progressPath(), data, 0o644)

	loaded, err := tracker.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loaded {
		t.Error("bad magic should not load")
	}
}

func TestProgressTracker_Load_BadVersion(t *testing.T) {
	dir := t.TempDir()
	n := makeTestNZB(1, 3)
	tracker := NewProgressTracker("badver", n, dir)

	data := make([]byte, headerSize+10)
	copy(data[0:4], progressMagic[:])
	data[4] = 99 // unsupported version
	os.WriteFile(tracker.progressPath(), data, 0o644)

	loaded, err := tracker.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loaded {
		t.Error("bad version should not load")
	}
}

func TestProgressTracker_Load_WrongFileCount(t *testing.T) {
	dir := t.TempDir()
	n := makeTestNZB(2, 3)
	tracker := NewProgressTracker("wrongfc", n, dir)

	data := make([]byte, headerSize+20)
	copy(data[0:4], progressMagic[:])
	data[4] = progressVersion
	binary.LittleEndian.PutUint16(data[6:8], 99) // wrong file count
	copy(data[8:40], tracker.fingerprint[:])
	os.WriteFile(tracker.progressPath(), data, 0o644)

	loaded, err := tracker.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loaded {
		t.Error("wrong file count should not load")
	}
}

func TestProgressTracker_Load_TruncatedBitset(t *testing.T) {
	dir := t.TempDir()
	n := makeTestNZB(1, 16)
	tracker := NewProgressTracker("truncbit", n, dir)

	// Build a valid header but truncate the bitset data
	data := make([]byte, headerSize+4) // header + segCount but no bitset
	copy(data[0:4], progressMagic[:])
	data[4] = progressVersion
	binary.LittleEndian.PutUint16(data[6:8], 1) // 1 file
	copy(data[8:40], tracker.fingerprint[:])
	binary.LittleEndian.PutUint32(data[headerSize:headerSize+4], 16) // 16 segs
	// No bitset data follows — truncated
	os.WriteFile(tracker.progressPath(), data, 0o644)

	loaded, err := tracker.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loaded {
		t.Error("truncated bitset should not load")
	}
}

func TestProgressTracker_Load_SegCountMismatch(t *testing.T) {
	dir := t.TempDir()
	n := makeTestNZB(1, 5)
	tracker := NewProgressTracker("segmis", n, dir)

	// Build valid header with correct file count and fingerprint, but wrong segCount
	bitsetLen := (999 + 7) / 8
	data := make([]byte, headerSize+4+bitsetLen)
	copy(data[0:4], progressMagic[:])
	data[4] = progressVersion
	binary.LittleEndian.PutUint16(data[6:8], 1)
	copy(data[8:40], tracker.fingerprint[:])
	binary.LittleEndian.PutUint32(data[headerSize:headerSize+4], 999) // wrong seg count
	os.WriteFile(tracker.progressPath(), data, 0o644)

	loaded, err := tracker.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loaded {
		t.Error("segment count mismatch should not load")
	}
}

func TestProgressTracker_Load_NonexistentFile(t *testing.T) {
	dir := t.TempDir()
	n := makeTestNZB(1, 3)
	tracker := NewProgressTracker("nofile", n, dir)

	loaded, err := tracker.Load()
	if err != nil {
		t.Fatalf("unexpected error for missing file: %v", err)
	}
	if loaded {
		t.Error("nonexistent file should return false")
	}
}

// --- Flush and Load round-trip with multiple files ---

func TestProgressTracker_MultiFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	n := makeTestNZB(5, 10) // 5 files, 10 segments each
	tracker := NewProgressTracker("multi-rt", n, dir)

	// Mark various segments across files
	tracker.MarkDone(0, 0)
	tracker.MarkDone(0, 9)
	tracker.MarkDone(1, 5)
	tracker.MarkDone(2, 0)
	tracker.MarkDone(2, 1)
	tracker.MarkDone(2, 2)
	tracker.MarkDone(2, 3)
	tracker.MarkDone(2, 4)
	tracker.MarkDone(2, 5)
	tracker.MarkDone(2, 6)
	tracker.MarkDone(2, 7)
	tracker.MarkDone(2, 8)
	tracker.MarkDone(2, 9) // file 2 fully done
	tracker.MarkDone(4, 7)

	if err := tracker.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// Reload
	tracker2 := NewProgressTracker("multi-rt", n, dir)
	loaded, err := tracker2.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !loaded {
		t.Fatal("should load")
	}

	if tracker2.CompletedSegments(0) != 2 {
		t.Errorf("file 0: got %d, want 2", tracker2.CompletedSegments(0))
	}
	if tracker2.CompletedSegments(1) != 1 {
		t.Errorf("file 1: got %d, want 1", tracker2.CompletedSegments(1))
	}
	if !tracker2.IsFileDone(2) {
		t.Error("file 2 should be done")
	}
	if tracker2.CompletedSegments(3) != 0 {
		t.Errorf("file 3: got %d, want 0", tracker2.CompletedSegments(3))
	}
	if tracker2.CompletedSegments(4) != 1 {
		t.Errorf("file 4: got %d, want 1", tracker2.CompletedSegments(4))
	}
	if tracker2.TotalCompleted() != 14 {
		t.Errorf("TotalCompleted: got %d, want 14", tracker2.TotalCompleted())
	}
}

// --- Concurrent mark + IsDone reads ---

func TestProgressTracker_ConcurrentMarkAndRead(t *testing.T) {
	dir := t.TempDir()
	segCount := 500
	n := makeTestNZB(2, segCount)
	tracker := NewProgressTracker("conc-rw", n, dir)

	// Use separate WaitGroups for writers and readers
	var writerWg sync.WaitGroup
	stop := make(chan struct{})

	// Writers
	for file := 0; file < 2; file++ {
		for seg := 0; seg < segCount; seg++ {
			writerWg.Add(1)
			go func(f, s int) {
				defer writerWg.Done()
				tracker.MarkDone(f, s)
			}(file, seg)
		}
	}

	// Readers — continuously read while writes happen
	var readerWg sync.WaitGroup
	for i := 0; i < 4; i++ {
		readerWg.Add(1)
		go func() {
			defer readerWg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					// These should never panic
					tracker.IsDone(0, 0)
					tracker.IsDone(1, segCount-1)
					tracker.IsFileDone(0)
					tracker.CompletedSegments(1)
					tracker.TotalCompleted()
				}
			}
		}()
	}

	// Wait for all writers to finish, then stop readers
	writerWg.Wait()
	close(stop)
	readerWg.Wait()

	// After all goroutines complete, everything should be done
	if !tracker.IsFileDone(0) {
		t.Errorf("file 0 should be done, got %d/%d", tracker.CompletedSegments(0), segCount)
	}
	if !tracker.IsFileDone(1) {
		t.Errorf("file 1 should be done, got %d/%d", tracker.CompletedSegments(1), segCount)
	}
}

// --- Concurrent flush safety ---

func TestProgressTracker_ConcurrentFlush(t *testing.T) {
	dir := t.TempDir()
	n := makeTestNZB(1, 100)
	tracker := NewProgressTracker("conc-flush", n, dir)

	// Mark some segments
	for i := 0; i < 50; i++ {
		tracker.MarkDone(0, i)
	}

	// Multiple concurrent flushes should not panic or corrupt
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tracker.Flush()
		}()
	}
	wg.Wait()

	// Verify state is loadable
	tracker2 := NewProgressTracker("conc-flush", n, dir)
	loaded, err := tracker2.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !loaded {
		t.Fatal("should load after concurrent flushes")
	}
	if tracker2.CompletedSegments(0) != 50 {
		t.Errorf("after concurrent flush: got %d, want 50", tracker2.CompletedSegments(0))
	}
}

// --- Remove with .tmp file ---

func TestProgressTracker_Remove_WithTmpFile(t *testing.T) {
	dir := t.TempDir()
	n := makeTestNZB(1, 3)
	tracker := NewProgressTracker("rm-tmp", n, dir)

	// Create all three files that Remove should clean up
	os.WriteFile(tracker.progressPath(), []byte("data"), 0o644)
	os.WriteFile(tracker.nzbPath(), []byte("<nzb/>"), 0o644)
	os.WriteFile(tracker.progressPath()+".tmp", []byte("tmp"), 0o644)

	tracker.Remove()

	for _, p := range []string{tracker.progressPath(), tracker.nzbPath(), tracker.progressPath() + ".tmp"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("file should be removed: %s", p)
		}
	}
}

// --- CleanStaleFiles edge cases ---

func TestCleanStaleFiles_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	if got := CleanStaleFiles(dir, time.Hour); got != 0 {
		t.Errorf("empty dir: got %d removed, want 0", got)
	}
}

func TestCleanStaleFiles_NonexistentDir(t *testing.T) {
	if got := CleanStaleFiles("/nonexistent/path/that/does/not/exist", time.Hour); got != 0 {
		t.Errorf("nonexistent dir: got %d removed, want 0", got)
	}
}

func TestCleanStaleFiles_AllFresh(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.progress"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.progress"), []byte("b"), 0o644)

	if got := CleanStaleFiles(dir, 24*time.Hour); got != 0 {
		t.Errorf("all fresh: got %d removed, want 0", got)
	}
}

func TestCleanStaleFiles_SkipsSubdirs(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "subdir")
	os.MkdirAll(subDir, 0o755)

	// Backdate the subdir (it should not be removed)
	os.Chtimes(subDir, fixedPast, fixedPast)

	if got := CleanStaleFiles(dir, 24*time.Hour); got != 0 {
		t.Errorf("should skip subdirs: got %d removed, want 0", got)
	}
	if _, err := os.Stat(subDir); err != nil {
		t.Error("subdir should still exist")
	}
}

func TestCleanStaleFiles_MixedAges(t *testing.T) {
	dir := t.TempDir()

	stale1 := filepath.Join(dir, "old1.progress")
	stale2 := filepath.Join(dir, "old2.nzb")
	fresh := filepath.Join(dir, "new.progress")

	os.WriteFile(stale1, []byte("x"), 0o644)
	os.WriteFile(stale2, []byte("x"), 0o644)
	os.WriteFile(fresh, []byte("x"), 0o644)

	os.Chtimes(stale1, fixedPast, fixedPast)
	os.Chtimes(stale2, fixedPast, fixedPast)

	if got := CleanStaleFiles(dir, 7*24*time.Hour); got != 2 {
		t.Errorf("mixed ages: got %d removed, want 2", got)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh file should still exist")
	}
}

// --- progressPath / nzbPath ---

func TestProgressTracker_Paths(t *testing.T) {
	dir := "/some/dir"
	n := makeTestNZB(1, 1)
	tracker := NewProgressTracker("my-task", n, dir)

	if got := tracker.progressPath(); got != filepath.Join(dir, "my-task.progress") {
		t.Errorf("progressPath: got %q", got)
	}
	if got := tracker.nzbPath(); got != filepath.Join(dir, "my-task.nzb") {
		t.Errorf("nzbPath: got %q", got)
	}
}

// --- formatBytes ---

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		input int64
		want  string
	}{
		{0, "0 B"},
		{500, "500 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{1073741824, "1.0 GB"},
		{1099511627776, "1.0 TB"},
	}
	for _, tt := range tests {
		got := formatBytes(tt.input)
		if got != tt.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// --- Single file with 1 segment (boundary) ---

func TestProgressTracker_SingleSegment(t *testing.T) {
	dir := t.TempDir()
	n := makeTestNZB(1, 1)
	tracker := NewProgressTracker("single-seg", n, dir)

	if tracker.IsFileDone(0) {
		t.Error("should not be done initially")
	}

	tracker.MarkDone(0, 0)

	if !tracker.IsFileDone(0) {
		t.Error("should be done after marking the only segment")
	}

	if err := tracker.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	tracker2 := NewProgressTracker("single-seg", n, dir)
	loaded, _ := tracker2.Load()
	if !loaded {
		t.Fatal("should load")
	}
	if !tracker2.IsFileDone(0) {
		t.Error("should be done after reload")
	}
}

// --- Flush creates directory if missing ---

func TestProgressTracker_FlushCreatesDir(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "nested", "resume")
	n := makeTestNZB(1, 2)
	tracker := NewProgressTracker("mkdir-test", n, dir)

	tracker.MarkDone(0, 0)
	if err := tracker.Flush(); err != nil {
		t.Fatalf("flush should create dir: %v", err)
	}

	if _, err := os.Stat(tracker.progressPath()); err != nil {
		t.Fatalf("progress file should exist: %v", err)
	}
}

// --- Double flush after no new marks ---

func TestProgressTracker_DoubleFlush(t *testing.T) {
	dir := t.TempDir()
	n := makeTestNZB(1, 3)
	tracker := NewProgressTracker("dbl-flush", n, dir)

	tracker.MarkDone(0, 0)
	if err := tracker.Flush(); err != nil {
		t.Fatalf("first flush: %v", err)
	}

	// Second flush without new marks should be a no-op (dirty=false)
	if err := tracker.Flush(); err != nil {
		t.Fatalf("second flush: %v", err)
	}
}

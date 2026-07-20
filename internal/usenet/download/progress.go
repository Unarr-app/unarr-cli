package download

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nzb"
)

// Binary progress file format:
//   [4B magic "UNRP"] [1B version=2] [1B reserved] [2B fileCount]
//   [32B SHA-256 fingerprint]
//   Per file: [4B segCount] [8B knownSize] [ceil(segCount/8) bytes bitset]
//
// v1 → v2 is a DELIBERATE hard break, not just a field addition. v1 progress
// files were written by an assembler that placed decoded segment data at
// ENCODED (yEnc, ~3% inflated) offsets, so every "done" bit in a v1 file
// vouches for bytes sitting at the wrong place in the partial file. Resuming
// from one would preserve that corruption forever. Load() rejects any version
// != progressVersion, so an old file silently degrades to a clean re-download.

var progressMagic = [4]byte{'U', 'N', 'R', 'P'}

const (
	progressVersion  = 2
	headerSize       = 4 + 1 + 1 + 2 + 32 // 40 bytes
	fileHeaderSize   = 4 + 8              // segCount + knownSize
	flushInterval    = 2 * time.Second
	flushSegmentFreq = 100 // flush every N segment completions
)

// fileProgress tracks completed segments for a single NZB file.
type fileProgress struct {
	segCount  int
	completed []byte // bitset: ceil(segCount/8) bytes
	doneCount atomic.Int32

	// knownSize is the highest end-offset written for this file so far, i.e.
	// the assembled size implied by the segments completed to date. It is
	// persisted because the final Truncate needs the REAL decoded size, and a
	// resumed run only observes the =ypart offsets of the segments it fetches
	// itself — if the tail segment landed in an earlier run, this is the only
	// record of where the file actually ends.
	knownSize atomic.Int64
}

// ProgressTracker tracks segment-level download progress for resumable usenet downloads.
// Thread-safe for concurrent use by multiple download workers.
type ProgressTracker struct {
	taskID      string
	fingerprint [32]byte
	dir         string // directory where progress files are stored
	files       []fileProgress

	mu        sync.Mutex
	dirty     bool
	lastFlush time.Time
	markCount int // marks since last flush

	// flushMu serializes ENTIRE flushes (snapshot + write + rename). Clearing
	// dirty under mu alone is not enough: two overlapping Flush calls snapshot
	// in order but can RENAME out of order, so a stale snapshot lands last and
	// silently loses completed segments on reload (a resumed download would
	// re-fetch them). Held across I/O on purpose; MarkDone's hot path only
	// takes mu, so marking never blocks on disk.
	flushMu sync.Mutex
}

// Fingerprint computes a SHA-256 hash from all message-IDs in the NZB.
// Used to validate that a progress file matches the same NZB content.
func Fingerprint(n *nzb.NZB) [32]byte {
	var ids []string
	for _, f := range n.Files {
		for _, s := range f.Segments {
			ids = append(ids, s.MessageID)
		}
	}
	sort.Strings(ids)

	h := sha256.New()
	for _, id := range ids {
		h.Write([]byte(id))
		h.Write([]byte{'\n'})
	}

	var fp [32]byte
	copy(fp[:], h.Sum(nil))
	return fp
}

// NewProgressTracker creates a tracker for the given NZB.
// The dir parameter is the base directory for resume files (e.g. DataDir()/resume).
func NewProgressTracker(taskID string, n *nzb.NZB, dir string) *ProgressTracker {
	files := make([]fileProgress, len(n.Files))
	for i, f := range n.Files {
		segCount := len(f.Segments)
		files[i] = fileProgress{
			segCount:  segCount,
			completed: make([]byte, (segCount+7)/8),
		}
	}

	return &ProgressTracker{
		taskID:      taskID,
		fingerprint: Fingerprint(n),
		dir:         dir,
		files:       files,
		lastFlush:   time.Now(),
	}
}

// progressPath returns the path to the binary progress file.
func (p *ProgressTracker) progressPath() string {
	return filepath.Join(p.dir, p.taskID+".progress")
}

// nzbPath returns the path to the cached NZB file.
func (p *ProgressTracker) nzbPath() string {
	return filepath.Join(p.dir, p.taskID+".nzb")
}

// Load reads a progress file from disk and validates its fingerprint.
// Returns true if the file was loaded successfully and matches the current NZB.
// Returns false if the file doesn't exist, is invalid, or has a different fingerprint.
func (p *ProgressTracker) Load() (bool, error) {
	data, err := os.ReadFile(p.progressPath())
	if err != nil {
		return false, nil // file doesn't exist = fresh start
	}

	if len(data) < headerSize {
		return false, nil
	}

	// Validate magic
	if data[0] != progressMagic[0] || data[1] != progressMagic[1] ||
		data[2] != progressMagic[2] || data[3] != progressMagic[3] {
		return false, nil
	}

	// Validate version
	if data[4] != progressVersion {
		return false, nil
	}

	// Validate file count
	fileCount := int(binary.LittleEndian.Uint16(data[6:8]))
	if fileCount != len(p.files) {
		return false, nil
	}

	// Validate fingerprint
	var storedFP [32]byte
	copy(storedFP[:], data[8:40])
	if storedFP != p.fingerprint {
		return false, nil
	}

	// Parse every file into locals FIRST, commit only once the whole payload
	// validates. Writing straight into p.files as we go left a rejected file
	// half-applied: a later "return false, nil" reads as a clean start to the
	// caller while the earlier entries kept their loaded knownSize, which then
	// seeded the final Truncate with a stale (possibly larger) size and padded
	// the file with zeros that par2 reports as damage.
	sizes := make([]int64, len(p.files))
	bitsets := make([][]byte, len(p.files))
	counts := make([]int32, len(p.files))

	offset := headerSize
	for i := range p.files {
		if offset+fileHeaderSize > len(data) {
			return false, nil
		}
		segCount := int(binary.LittleEndian.Uint32(data[offset : offset+4]))
		sizes[i] = int64(binary.LittleEndian.Uint64(data[offset+4 : offset+fileHeaderSize]))
		offset += fileHeaderSize

		if segCount != p.files[i].segCount {
			return false, nil
		}

		bitsetLen := (segCount + 7) / 8
		if offset+bitsetLen > len(data) {
			return false, nil
		}
		bitsets[i] = data[offset : offset+bitsetLen]
		offset += bitsetLen

		// Count completed segments
		for seg := 0; seg < segCount; seg++ {
			if bitsets[i][seg/8]&(1<<uint(seg%8)) != 0 {
				counts[i]++
			}
		}
	}

	for i := range p.files {
		copy(p.files[i].completed, bitsets[i])
		p.files[i].doneCount.Store(counts[i])
		p.files[i].knownSize.Store(sizes[i])
	}

	return true, nil
}

// MarkDone marks a segment as completed. Thread-safe.
// Automatically flushes to disk periodically.
func (p *ProgressTracker) MarkDone(fileIndex, segIndex int) {
	if fileIndex < 0 || fileIndex >= len(p.files) {
		return
	}
	fp := &p.files[fileIndex]
	if segIndex < 0 || segIndex >= fp.segCount {
		return
	}

	p.mu.Lock()
	mask := byte(1 << uint(segIndex%8))
	alreadyDone := fp.completed[segIndex/8]&mask != 0
	if !alreadyDone {
		fp.completed[segIndex/8] |= mask
		fp.doneCount.Add(1)
		p.dirty = true
		p.markCount++
	}

	shouldFlush := !alreadyDone && (p.markCount >= flushSegmentFreq || time.Since(p.lastFlush) >= flushInterval)
	p.mu.Unlock()

	if shouldFlush {
		p.tryFlush()
	}
}

// tryFlush is the cadence-triggered flush for the download-worker hot path:
// if another flush is already mid-I/O it SKIPS instead of parking the worker
// behind a disk write (dirty stays set, so the next cadence or the final
// explicit Flush persists the state — same durability window, no convoy).
func (p *ProgressTracker) tryFlush() {
	if !p.flushMu.TryLock() {
		return
	}
	defer p.flushMu.Unlock()
	p.flushLocked()
}

// IsDone returns whether a specific segment has been completed.
func (p *ProgressTracker) IsDone(fileIndex, segIndex int) bool {
	if fileIndex < 0 || fileIndex >= len(p.files) {
		return false
	}
	fp := &p.files[fileIndex]
	if segIndex < 0 || segIndex >= fp.segCount {
		return false
	}
	p.mu.Lock()
	done := fp.completed[segIndex/8]&(1<<uint(segIndex%8)) != 0
	p.mu.Unlock()
	return done
}

// IsFileDone returns true if all segments of a file are completed.
func (p *ProgressTracker) IsFileDone(fileIndex int) bool {
	if fileIndex < 0 || fileIndex >= len(p.files) {
		return false
	}
	fp := &p.files[fileIndex]
	return int(fp.doneCount.Load()) >= fp.segCount
}

// CompletedSegments returns the number of completed segments for a file.
func (p *ProgressTracker) CompletedSegments(fileIndex int) int {
	if fileIndex < 0 || fileIndex >= len(p.files) {
		return 0
	}
	return int(p.files[fileIndex].doneCount.Load())
}

// CompletedBytes returns the total bytes of completed segments for a file.
func (p *ProgressTracker) CompletedBytes(fileIndex int, segments []nzb.Segment) int64 {
	if fileIndex < 0 || fileIndex >= len(p.files) {
		return 0
	}
	var total int64
	for i, seg := range segments {
		if p.IsDone(fileIndex, i) {
			total += seg.Bytes
		}
	}
	return total
}

// NoteFileSize records that data was written up to end (exclusive) in the
// given file, keeping the running maximum. Callers pass the end offset derived
// from the yEnc =ypart header, so the tracked value converges on the file's
// true decoded size as segments land — in any order, across runs.
func (p *ProgressTracker) NoteFileSize(fileIndex int, end int64) {
	if fileIndex < 0 || fileIndex >= len(p.files) || end <= 0 {
		return
	}
	fp := &p.files[fileIndex]
	for {
		cur := fp.knownSize.Load()
		if end <= cur {
			return
		}
		if fp.knownSize.CompareAndSwap(cur, end) {
			p.mu.Lock()
			p.dirty = true
			p.mu.Unlock()
			return
		}
	}
}

// KnownSize returns the highest end-offset recorded for a file, or 0 when
// nothing has been written yet.
func (p *ProgressTracker) KnownSize(fileIndex int) int64 {
	if fileIndex < 0 || fileIndex >= len(p.files) {
		return 0
	}
	return p.files[fileIndex].knownSize.Load()
}

// ResetFile clears every completed bit (and the recorded size) for one file, so
// the next run re-fetches it from scratch while the REST of the NZB keeps its
// resume state. This is what makes an integrity retry honest: par2 reports
// damage per file, so only the files it names are discarded — a 40 GB release
// whose single bad RAR volume needs re-fetching does not re-download the other
// 39 GB, and the retry is genuinely clean rather than a no-op replay of the
// same bits (the old behaviour: every segment still marked done, so the
// "re-download clean" pass fetched nothing and failed par2 identically 3×).
func (p *ProgressTracker) ResetFile(fileIndex int) {
	if fileIndex < 0 || fileIndex >= len(p.files) {
		return
	}
	fp := &p.files[fileIndex]
	p.mu.Lock()
	for i := range fp.completed {
		fp.completed[i] = 0
	}
	fp.doneCount.Store(0)
	fp.knownSize.Store(0)
	p.dirty = true
	p.mu.Unlock()
}

// TotalCompleted returns total completed segments across all files.
func (p *ProgressTracker) TotalCompleted() int {
	var total int
	for i := range p.files {
		total += int(p.files[i].doneCount.Load())
	}
	return total
}

// Flush writes the current progress state to disk atomically (tmp + rename),
// BLOCKING until any in-flight flush finishes — flushMu (not the dirty flag)
// is what serializes snapshot+write+rename so a stale snapshot can never land
// last. Use for explicit end-of-download/cancel persistence; the MarkDone hot
// path goes through tryFlush instead. If I/O fails, dirty is re-set by the
// next MarkDone.
func (p *ProgressTracker) Flush() error {
	p.flushMu.Lock()
	defer p.flushMu.Unlock()
	return p.flushLocked()
}

// flushLocked does the snapshot + atomic write. Caller MUST hold flushMu.
func (p *ProgressTracker) flushLocked() error {
	p.mu.Lock()
	if !p.dirty {
		p.mu.Unlock()
		return nil
	}

	// Snapshot state and clear dirty while holding the lock. Serialization
	// against other flushes is flushMu's job (held by our caller); clearing
	// dirty here only marks the snapshotted work as persisted-in-progress.
	size := headerSize
	for i := range p.files {
		size += fileHeaderSize + (p.files[i].segCount+7)/8
	}

	buf := make([]byte, size)

	// Header
	copy(buf[0:4], progressMagic[:])
	buf[4] = progressVersion
	buf[5] = 0 // reserved
	binary.LittleEndian.PutUint16(buf[6:8], uint16(len(p.files)))
	copy(buf[8:40], p.fingerprint[:])

	// Per-file bitsets
	offset := headerSize
	for i := range p.files {
		fp := &p.files[i]
		binary.LittleEndian.PutUint32(buf[offset:offset+4], uint32(fp.segCount))
		binary.LittleEndian.PutUint64(buf[offset+4:offset+fileHeaderSize], uint64(fp.knownSize.Load()))
		offset += fileHeaderSize
		bitsetLen := (fp.segCount + 7) / 8
		copy(buf[offset:offset+bitsetLen], fp.completed[:bitsetLen])
		offset += bitsetLen
	}

	p.dirty = false
	p.markCount = 0
	p.lastFlush = time.Now()
	p.mu.Unlock()

	// Atomic write: tmp file + rename (outside lock — I/O is slow)
	if err := os.MkdirAll(p.dir, 0o755); err != nil {
		return fmt.Errorf("create resume dir: %w", err)
	}

	tmpPath := p.progressPath() + ".tmp"
	if err := os.WriteFile(tmpPath, buf, 0o644); err != nil {
		p.markDirty()
		return fmt.Errorf("write progress tmp: %w", err)
	}

	if err := os.Rename(tmpPath, p.progressPath()); err != nil {
		os.Remove(tmpPath)
		p.markDirty()
		return fmt.Errorf("rename progress: %w", err)
	}

	return nil
}

// markDirty re-arms the dirty flag after a failed write. flushLocked clears it
// at snapshot time, before the I/O, so without this a flush that fails (ENOSPC
// is entirely plausible on the very path that handles a failed download) leaves
// dirty=false and turns every later flush into a no-op — silently discarding
// the snapshot. That matters most for ResetFile: a lost invalidation means the
// retry reloads an all-done bitset and re-downloads nothing.
func (p *ProgressTracker) markDirty() {
	p.mu.Lock()
	p.dirty = true
	p.mu.Unlock()
}

// Remove deletes both the progress file and cached NZB file (best-effort).
// It waits for any in-flight flush (flushMu) and clears dirty first —
// otherwise a cancel-path Flush racing Remove could re-write the progress
// file AFTER deletion, and a re-added task would resume from segments whose
// partial data files no longer exist.
func (p *ProgressTracker) Remove() {
	p.flushMu.Lock()
	defer p.flushMu.Unlock()
	p.mu.Lock()
	p.dirty = false
	p.mu.Unlock()
	os.Remove(p.progressPath())
	os.Remove(p.nzbPath())
	os.Remove(p.progressPath() + ".tmp")
}

// CleanStaleFiles removes resume files older than maxAge from the given directory.
func CleanStaleFiles(dir string, maxAge time.Duration) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}

	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if time.Since(info.ModTime()) > maxAge {
			if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
				removed++
			}
		}
	}
	return removed
}

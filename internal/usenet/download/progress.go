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

	"github.com/torrentclaw/unarr/internal/usenet/nzb"
)

// Binary progress file format:
//   [4B magic "UNRP"] [1B version=1] [1B reserved] [2B fileCount]
//   [32B SHA-256 fingerprint]
//   Per file: [4B segCount] [ceil(segCount/8) bytes bitset]

var progressMagic = [4]byte{'U', 'N', 'R', 'P'}

const (
	progressVersion  = 1
	headerSize       = 4 + 1 + 1 + 2 + 32 // 40 bytes
	flushInterval    = 2 * time.Second
	flushSegmentFreq = 100 // flush every N segment completions
)

// fileProgress tracks completed segments for a single NZB file.
type fileProgress struct {
	segCount  int
	completed []byte // bitset: ceil(segCount/8) bytes
	doneCount atomic.Int32
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

	// Read per-file bitsets
	offset := headerSize
	for i := range p.files {
		if offset+4 > len(data) {
			return false, nil
		}
		segCount := int(binary.LittleEndian.Uint32(data[offset : offset+4]))
		offset += 4

		if segCount != p.files[i].segCount {
			return false, nil
		}

		bitsetLen := (segCount + 7) / 8
		if offset+bitsetLen > len(data) {
			return false, nil
		}

		copy(p.files[i].completed, data[offset:offset+bitsetLen])
		offset += bitsetLen

		// Count completed segments
		var count int32
		for seg := 0; seg < segCount; seg++ {
			if p.files[i].completed[seg/8]&(1<<uint(seg%8)) != 0 {
				count++
			}
		}
		p.files[i].doneCount.Store(count)
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
		p.Flush()
	}
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

// TotalCompleted returns total completed segments across all files.
func (p *ProgressTracker) TotalCompleted() int {
	var total int
	for i := range p.files {
		total += int(p.files[i].doneCount.Load())
	}
	return total
}

// Flush writes the current progress state to disk atomically (tmp + rename).
// dirty is cleared before I/O to prevent concurrent Flush calls from racing
// on the same tmp file. If I/O fails, the next MarkDone will re-set dirty.
func (p *ProgressTracker) Flush() error {
	p.mu.Lock()
	if !p.dirty {
		p.mu.Unlock()
		return nil
	}

	// Snapshot state and clear dirty while holding the lock.
	// This serializes flushes: a concurrent MarkDone will set dirty=true
	// again, but won't trigger a competing Flush on the same tmp file.
	size := headerSize
	for i := range p.files {
		size += 4 + (p.files[i].segCount+7)/8
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
		offset += 4
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
		return fmt.Errorf("write progress tmp: %w", err)
	}

	if err := os.Rename(tmpPath, p.progressPath()); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename progress: %w", err)
	}

	return nil
}

// Remove deletes both the progress file and cached NZB file (best-effort).
func (p *ProgressTracker) Remove() {
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

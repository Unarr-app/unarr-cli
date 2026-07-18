// Package stream builds the byte-range machinery that lets the Usenet path
// serve a video WHILE it downloads, instead of fetching the whole file first.
//
// OffsetIndex is the foundational piece: it maps a byte position in the logical
// file to the NNTP article (yEnc part) that carries it. It starts from a cheap,
// network-free ESTIMATE (accumulated Segment.Bytes) and refines itself to
// byte-EXACT as articles are fetched and their yEnc =ypart begin/end headers are
// observed. For the common uniform-part posting a single observation pins the
// entire map; for irregular postings every observed segment pins itself and its
// neighbours interpolate until they too are observed.
package stream

import (
	"log"
	"sort"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nzb"
	"github.com/Unarr-app/unarr-cli/internal/usenet/yenc"
)

// OffsetIndex resolves a file-byte offset to the segment (yEnc article) holding
// it. It is a pure, in-memory structure: no network, no goroutines. Construct
// with NewOffsetIndex, refine with Observe as parts are decoded, query with
// Locate. It is NOT safe for concurrent use; the owning Reader serialises access.
type OffsetIndex struct {
	segs []nzb.Segment // sorted ascending by part Number

	estLen     []int64 // per-segment ESTIMATE from Segment.Bytes (encoded size)
	exactLen   []int64 // exact decoded length once observed, else -1
	exactStart []int64 // exact 0-based file start once observed, else -1

	// uniformStep is the exact decoded length of the first observed NON-final
	// segment. In the common uniform posting this is the shared part size, so
	// one observation lets every not-yet-observed non-final segment inherit an
	// exact length. Zero until such a segment is observed.
	uniformStep int64

	fileSize  int64 // exact total once sizeExact, else the running estimate
	sizeExact bool

	// Cached layout, rebuilt by recompute() on construction and every Observe.
	starts []int64 // 0-based start offset of each segment (monotonic)
}

// NewOffsetIndex builds an index over a single NZB file. Segments are copied and
// sorted by part Number; the initial layout is estimated from Segment.Bytes and
// is progressively corrected by Observe. A file with no segments yields an empty
// index whose Locate always reports not-found and whose FileSize is 0.
func NewOffsetIndex(f nzb.File) *OffsetIndex {
	segs := nzb.SortSegmentsByNumber(f.Segments)
	n := len(segs)

	ix := &OffsetIndex{
		segs:       segs,
		estLen:     make([]int64, n),
		exactLen:   make([]int64, n),
		exactStart: make([]int64, n),
		starts:     make([]int64, n),
	}
	var est int64
	for i, s := range segs {
		length := s.Bytes
		if length < 0 {
			length = 0
		}
		ix.estLen[i] = length
		ix.exactLen[i] = -1
		ix.exactStart[i] = -1
		est += length
	}
	ix.fileSize = est // estimate until an =ybegin size= is observed
	ix.recompute()
	return ix
}

// SegmentCount returns the number of segments (articles) in the file.
func (ix *OffsetIndex) SegmentCount() int { return len(ix.segs) }

// Segment returns the i-th segment in assembly order (sorted by part Number).
// It is the message-id source the Reader fetches. Callers must keep i in
// [0, SegmentCount()).
func (ix *OffsetIndex) Segment(i int) nzb.Segment { return ix.segs[i] }

// FileSize returns the best-known total size of the logical file: exact once any
// article's yEnc header (size=) has been Observed, otherwise the accumulated
// Segment.Bytes estimate (which runs ~3% high because those are encoded sizes).
func (ix *OffsetIndex) FileSize() int64 {
	if ix.sizeExact {
		return ix.fileSize
	}
	return ix.estimatedSize()
}

// SizeExact reports whether FileSize is byte-exact (an article header has been
// observed). The Reader uses this to decide whether a Seek to the end can be
// answered from the current map or needs one article fetched first.
func (ix *OffsetIndex) SizeExact() bool { return ix.sizeExact }

// Locate resolves a file offset to the segment carrying it and that segment's
// current best-known byte range [start, end). ok is false for a negative offset,
// an offset at or beyond FileSize (EOF), or an empty index. The returned range
// is exact once the segment has been Observed (and, for uniform postings, once
// any single segment has been observed).
func (ix *OffsetIndex) Locate(offset int64) (segIdx int, start, end int64, ok bool) {
	n := len(ix.segs)
	if n == 0 || offset < 0 || offset >= ix.FileSize() {
		return 0, 0, 0, false
	}
	// Largest i with starts[i] <= offset. starts is monotonic non-decreasing.
	i := sort.Search(n, func(k int) bool { return ix.starts[k] > offset }) - 1
	if i < 0 {
		i = 0
	}
	return i, ix.starts[i], ix.endOf(i), true
}

// Observe fixes the exact bounds of segment segIdx from a decoded yEnc part and
// rebuilds the layout. part.Begin/End are 1-based inclusive file offsets and
// part.Size is the whole-file size — the authoritative data the estimate is
// replaced with. Invalid input (nil part, out-of-range index, non-positive
// begin, end < begin) is logged and ignored rather than corrupting the map.
func (ix *OffsetIndex) Observe(segIdx int, part *yenc.Part) {
	if part == nil {
		log.Printf("[usenet-stream] offset-index: nil part for segment %d ignored", segIdx)
		return
	}
	if segIdx < 0 || segIdx >= len(ix.segs) {
		log.Printf("[usenet-stream] offset-index: segment %d out of range [0,%d) ignored", segIdx, len(ix.segs))
		return
	}
	if part.Begin < 1 || part.End < part.Begin {
		log.Printf("[usenet-stream] offset-index: segment %d bad ypart begin=%d end=%d ignored",
			segIdx, part.Begin, part.End)
		return
	}

	ix.exactStart[segIdx] = part.Begin - 1 // 1-based inclusive -> 0-based
	ix.exactLen[segIdx] = part.End - part.Begin + 1
	if part.Size > 0 {
		ix.fileSize = part.Size
		ix.sizeExact = true
	}
	if segIdx < len(ix.segs)-1 && ix.uniformStep == 0 {
		ix.uniformStep = ix.exactLen[segIdx]
	}
	ix.recompute()
}

// --- internal layout ---

// stepLen is the length used to advance from segment i to i+1: its exact decoded
// length when observed, the inferred uniform part size for an unobserved
// non-final segment, otherwise the encoded-size estimate.
func (ix *OffsetIndex) stepLen(i int) int64 {
	if ix.exactLen[i] >= 0 {
		return ix.exactLen[i]
	}
	if i < len(ix.segs)-1 && ix.uniformStep > 0 {
		return ix.uniformStep
	}
	return ix.estLen[i]
}

// recompute rebuilds starts[] from the current exact anchors and step lengths.
// An observed segment pins its exact start; every other segment accumulates from
// its predecessor. A monotonic guard keeps the array searchable even if a bad
// estimate would otherwise place a later start below an earlier one.
func (ix *OffsetIndex) recompute() {
	n := len(ix.segs)
	if n == 0 {
		return
	}
	ix.starts[0] = 0 // the first article always begins the logical file
	for i := 1; i < n; i++ {
		if ix.exactStart[i] >= 0 {
			ix.starts[i] = ix.exactStart[i]
		} else {
			ix.starts[i] = ix.starts[i-1] + ix.stepLen(i-1)
		}
		if ix.starts[i] < ix.starts[i-1] {
			ix.starts[i] = ix.starts[i-1]
		}
	}
}

// endOf returns the exclusive end offset of segment i: the next segment's start,
// or the file size for the final segment.
func (ix *OffsetIndex) endOf(i int) int64 {
	if i < len(ix.segs)-1 {
		return ix.starts[i+1]
	}
	return ix.FileSize()
}

// estimatedSize returns the running size estimate: the last segment's start plus
// its step length. Used until an article header supplies the exact size.
func (ix *OffsetIndex) estimatedSize() int64 {
	n := len(ix.segs)
	if n == 0 {
		return 0
	}
	return ix.starts[n-1] + ix.stepLen(n-1)
}

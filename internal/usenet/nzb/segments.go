package nzb

import "sort"

// SortSegmentsByNumber returns a copy of segs ordered ascending by part Number.
// It never mutates the input, so callers can safely order a File's segments for
// assembly without disturbing the parsed NZB. This is the ordering both the
// batch assembler and the streaming OffsetIndex build their byte layout on.
func SortSegmentsByNumber(segs []Segment) []Segment {
	out := make([]Segment, len(segs))
	copy(out, segs)
	sort.Slice(out, func(i, j int) bool {
		return out[i].Number < out[j].Number
	})
	return out
}

// EstimatedByteOffsets returns, for segments already in assembly order, the
// running start offset of each: offsets[i] == sum of Bytes of segments[0..i-1].
//
// These offsets are byte-EXACT only when Segment.Bytes equals the decoded
// payload size of each article. In practice Segment.Bytes is the ENCODED
// (yEnc, ~3% larger) article size, so this is an ESTIMATE: the batch assembler
// tolerates it (it WriteAt's decoded data and truncates to the real size at the
// end), and the streaming OffsetIndex refines it to exact using the yEnc
// =ypart begin/end headers observed while fetching. Never treat the result as
// authoritative file offsets on its own.
func EstimatedByteOffsets(segs []Segment) []int64 {
	offsets := make([]int64, len(segs))
	var off int64
	for i, s := range segs {
		offsets[i] = off
		off += s.Bytes
	}
	return offsets
}

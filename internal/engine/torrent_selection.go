// Package engine — torrent_selection.go owns what a torrent download ASKED FOR
// and how "is it really finished?" is measured against that.
//
// selectFiles downloads only the largest video + matching subtitles, so a
// multi-file torrent is deliberately left partial. Every completion check has to
// be phrased in terms of the selection; asking the torrent-wide accounting
// instead reports the skipped files as damage. See missingSelectedBytes.
package engine

import "github.com/anacrolix/torrent"

// selection is what selectFiles decided to download: the byte total to report
// as the task's size, the primary file name, and the files themselves.
//
// files == nil means "everything" (t.DownloadAll() was called, for a single-file
// torrent or one with no video). In that case whole-torrent accounting is the
// right accounting and t.BytesMissing() is used unchanged.
type selection struct {
	totalBytes int64
	fileName   string
	files      []*torrent.File
}

// fileExtent is a file's placement inside the torrent's byte stream — all the
// integrity maths needs, and free of any anacrolix type so it can be tested
// without a live client.
type fileExtent struct {
	offset int64
	length int64
}

// missingBytes reports how many bytes of what we ASKED FOR are still missing,
// counting ONLY SHA1-verified pieces as present.
//
// It must not be expressed as t.BytesMissing(). In anacrolix v1.61.0 that is
// bytesLeft(), which iterates every piece of the WHOLE torrent (iterFlipped over
// 0..numPieces()) and knows nothing about the file selection — so for a
// selective download it returns the size of everything we deliberately skipped.
// The completion guard therefore fired on healthy downloads: 32 failures over 12
// distinct users in 14 days, 15% of all torrent failures, each one a finished
// download thrown away and re-downloaded in full (and each retry's t.Drop()
// with live peers is precisely the mmap-span race storage_closeguard.go exists
// to survive). Control against prod: the reported "missing" tracked
// torrent.size_bytes minus the selected size — Chucky 4915 MB vs 4940 MB of
// unselected files, Iron Man 4198 vs 4193, Paquita Salas on three seasons.
//
// It must not be expressed as the sum of File.BytesCompleted() either, tempting
// as that looks: that subtracts numDirtyBytes() of INCOMPLETE pieces
// (anacrolix file.go, fileBytesLeft), i.e. it credits chunks that arrived but
// were never hash-checked — the exact bytes this guard exists to catch. Only
// PieceState.Complete, which reads the verified-piece bitmap, is consulted.
func (s selection) missingBytes(t *torrent.Torrent) int64 {
	if s.files == nil {
		return t.BytesMissing()
	}
	info := t.Info()
	if info == nil {
		return 0 // no metadata: nothing can be asserted, and the caller already has it
	}
	extents := make([]fileExtent, 0, len(s.files))
	for _, f := range s.files {
		extents = append(extents, fileExtent{offset: f.Offset(), length: f.Length()})
	}
	return missingSelectedBytes(info.PieceLength, extents, func(i int) bool {
		return t.PieceState(i).Complete
	})
}

// missingSelectedBytes sums, for every selected file, the bytes of that file
// that lie in pieces which are not verified. Selected extents are disjoint, so
// a piece shared by two of them contributes each file's own share exactly once.
func missingSelectedBytes(pieceLength int64, files []fileExtent, pieceVerified func(int) bool) int64 {
	if pieceLength <= 0 {
		return 0
	}
	var missing int64
	for _, f := range files {
		if f.length <= 0 {
			continue
		}
		start, end := f.offset, f.offset+f.length
		first := int(start / pieceLength)
		last := int((end - 1) / pieceLength)
		for i := first; i <= last; i++ {
			if pieceVerified(i) {
				continue
			}
			pieceStart := int64(i) * pieceLength
			missing += overlapBytes(start, end, pieceStart, pieceStart+pieceLength)
		}
	}
	return missing
}

// overlapBytes is the length of the intersection of [aStart,aEnd) and
// [bStart,bEnd), or 0 when they don't meet.
func overlapBytes(aStart, aEnd, bStart, bEnd int64) int64 {
	lo := max(aStart, bStart)
	hi := min(aEnd, bEnd)
	if hi <= lo {
		return 0
	}
	return hi - lo
}

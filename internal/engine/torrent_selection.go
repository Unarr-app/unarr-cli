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
// torrent or one with no video); missingBytes then measures every file, which is
// the same thing said a different way.
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

// completedBytes is how much of the SELECTION is verified on disk.
//
// This is what the download is "done" when it reaches, and it must be measured
// on the same scale as totalBytes. t.BytesCompleted() is not: it counts verified
// bytes across the WHOLE torrent, including pieces of files we never selected
// (boundary pieces shared with a skipped file are downloaded in full, and a
// resumed torrent can carry more). Compared against a selection-sized total it
// crosses the line early — measured on a real client at 360448 >= 262144 with
// 37768 bytes of the selection still missing — so the loop called a healthy,
// merely-unfinished download complete and the integrity guard then failed it.
// That is the same mixing of scales this file exists to remove, just with a
// smaller number, and it repeats on every retry because the unselected verified
// bytes survive and re-satisfy the test on the first tick.
func (s selection) completedBytes(t *torrent.Torrent) int64 {
	return s.totalBytes - s.missingBytes(t)
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
// It must NOT delegate to t.BytesMissing() for the "everything" case either.
// At the only point this is called — the poll loop has just seen
// BytesCompleted >= totalBytes, and totalBytes there IS t.Length() — that value
// is algebraically zero (BytesMissing = Length - BytesCompleted), so the guard
// would be dead for the commonest case of all, a single-file movie. The same
// piece-accurate accounting is used for every torrent; "everything" is simply
// every file.
func (s selection) missingBytes(t *torrent.Torrent) int64 {
	info := t.Info()
	if info == nil {
		return 0 // no metadata: nothing can be asserted, and the caller already knows
	}
	files := s.files
	if files == nil {
		files = t.Files()
	}
	extents := make([]fileExtent, 0, len(files))
	for _, f := range files {
		extents = append(extents, fileExtent{offset: f.Offset(), length: f.Length()})
	}
	return missingSelectedBytes(info.PieceLength, extents, verifiedPieceLookup(t))
}

// verifiedPieceLookup snapshots which pieces are verified in ONE client-lock
// acquisition and returns a lookup over that snapshot.
//
// Deliberately not `func(i int) bool { return t.PieceState(i).Complete }`:
// PieceState takes the client read lock per call, which is ~61k acquisitions
// for a 60 GB selection at 1 MB pieces. torrent.go's waitPieceMarkingSettled
// already documents that exact anti-pattern (it measured ~15k lock round-trips
// per poll) and PieceStateRuns is the single-lock alternative it settled on.
func verifiedPieceLookup(t *torrent.Torrent) func(int) bool {
	verified := make([]bool, t.NumPieces())
	i := 0
	for _, run := range t.PieceStateRuns() {
		for n := 0; n < run.Length && i < len(verified); n++ {
			verified[i] = run.Complete
			i++
		}
	}
	return func(idx int) bool {
		return idx >= 0 && idx < len(verified) && verified[idx]
	}
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

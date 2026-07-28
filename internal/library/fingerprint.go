package library

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
)

// fpChunk is how many bytes are hashed from the head and the tail of a file.
const fpChunk = 1 << 20 // 1 MiB

// ComputeFingerprint returns a stable content identity for a media file:
// sha256(fileSize ‖ first 1 MiB ‖ last 1 MiB). It survives renames, moves, and
// base-path changes (unlike the absolute path), so the server can recognise the
// same file at a new location and move its library row in place instead of
// duplicating it. Cheap: two bounded reads, never the whole file (except small
// ones). See docs/plans/unarr-path-resilience.md in the web repo.
func ComputeFingerprint(path string, size int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	var sizeBuf [8]byte
	binary.LittleEndian.PutUint64(sizeBuf[:], uint64(size))
	h.Write(sizeBuf[:])

	if size <= 2*fpChunk {
		// Small file: hash it whole — head+tail would overlap anyway.
		if _, err := io.Copy(h, f); err != nil {
			return "", err
		}
	} else {
		head := make([]byte, fpChunk)
		if _, err := io.ReadFull(f, head); err != nil {
			return "", err
		}
		h.Write(head)

		if _, err := f.Seek(size-fpChunk, io.SeekStart); err != nil {
			return "", err
		}
		tail := make([]byte, fpChunk)
		if _, err := io.ReadFull(f, tail); err != nil {
			return "", err
		}
		h.Write(tail)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// FirstOrLastMiBAllZero reports whether the first 1 MiB AND/OR the last 1 MiB of a
// file are entirely NUL bytes. It reuses the SAME bounded head/tail reads that
// ComputeFingerprint performs (first + last 1 MiB) — a corrupt "zero-content"
// download (right size, but the media payload is a hole of zeros) is unplayable
// yet passes the size floor and gets a fingerprint that differs from the real
// copy, so the dedup never collapses it. This flags it.
//
// STRONG but NOT ABSOLUTE: a legitimate media file could in theory begin (or end)
// with a 1 MiB run of zeros, so the reconcile category that consumes this is gated
// OFF by default and only ever removed by an explicit manual `library clean
// --apply` — never by the daemon's automatic sweep.
//
// A file <= 2*fpChunk is checked whole (its head and tail overlap); a read error
// returns (false, err) — the caller treats "can't prove corrupt" as not corrupt.
func FirstOrLastMiBAllZero(path string, size int64) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	if size <= 2*fpChunk {
		data, err := io.ReadAll(f)
		if err != nil {
			return false, err
		}
		return len(data) > 0 && isAllZero(data), nil
	}

	head := make([]byte, fpChunk)
	if _, err := io.ReadFull(f, head); err != nil {
		return false, err
	}
	if isAllZero(head) {
		return true, nil
	}

	if _, err := f.Seek(size-fpChunk, io.SeekStart); err != nil {
		return false, err
	}
	tail := make([]byte, fpChunk)
	if _, err := io.ReadFull(f, tail); err != nil {
		return false, err
	}
	return isAllZero(tail), nil
}

// isAllZero reports whether every byte in b is NUL. Cheap linear scan; returns
// false for an empty slice (nothing to judge).
func isAllZero(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// sameContentBufSize is the block size for the streaming full compare.
const sameContentBufSize = 1 << 20 // 1 MiB

// SameFileContent reports whether two files are byte-for-byte identical by
// streaming both and comparing block by block. Unlike ComputeFingerprint — which
// only samples size + first/last 1 MiB and can therefore COLLIDE for two files
// that match on their extremes but differ in the MIDDLE — this is an exact answer.
//
// It is the confirmation step before ANY automatic, unconfirmed delete
// (reconcile's auto-dedup, organize's redundant-copy removal): the fingerprint is
// a cheap FILTER to find candidates; identity is proven here. The full-read cost
// is only paid on a fingerprint collision (rare), exactly when being wrong would
// destroy data. A size mismatch short-circuits immediately (different content).
func SameFileContent(a, b string) (bool, error) {
	if a == b {
		return true, nil // same path — trivially identical
	}
	fa, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer fb.Close()

	// Different size → definitely different content (cheap short-circuit).
	sa, err := fa.Stat()
	if err != nil {
		return false, err
	}
	sb, err := fb.Stat()
	if err != nil {
		return false, err
	}
	if sa.Size() != sb.Size() {
		return false, nil
	}
	return streamsEqual(fa, fb)
}

// streamsEqual compares two readers block-by-block. Callers have already confirmed
// equal length, so a difference in bytes read or content means "different".
func streamsEqual(ra, rb io.Reader) (bool, error) {
	bufA := make([]byte, sameContentBufSize)
	bufB := make([]byte, sameContentBufSize)
	for {
		na, ea := readBlock(ra, bufA)
		if ea != nil && ea != io.EOF {
			return false, ea
		}
		nb, eb := readBlock(rb, bufB)
		if eb != nil && eb != io.EOF {
			return false, eb
		}
		if na != nb || !bytes.Equal(bufA[:na], bufB[:nb]) {
			return false, nil
		}
		if ea == io.EOF { // equal length → both end together
			return true, nil
		}
	}
}

// readBlock fills buf as far as possible, normalising io.ReadFull's short-read
// signal (ErrUnexpectedEOF) to a plain io.EOF so callers see one end-of-stream code.
func readBlock(r io.Reader, buf []byte) (int, error) {
	n, err := io.ReadFull(r, buf)
	if err == io.ErrUnexpectedEOF {
		return n, io.EOF
	}
	return n, err
}

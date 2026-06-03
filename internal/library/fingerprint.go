package library

import (
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

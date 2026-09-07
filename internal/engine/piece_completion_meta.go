package engine

import (
	"encoding/binary"
	"io"
	"os"
)

// Reads just enough of a bolt file's two meta pages to decide whether the
// out-of-process integrity check has to run at all — see
// pieceCompletionNeedsCheck. No bbolt import: this must never open, lock or
// mmap the file.
//
// Layout (bbolt internal/common, little-endian on every target we ship):
//
//	page header: id u64 @0 · flags u16 @8 · count u16 @10 · overflow u32 @12 · data @16
//	meta (@16 in pages 0 and 1): magic u32 · version u32 · pageSize u32 · flags u32 ·
//	  root.pgid u64 @16 · root.seq u64 @24 · freelist pgid u64 @32 · pgid u64 @40 · txid u64 @48
const (
	boltMagic          = 0xED0CDAED
	boltPageHeaderSize = 16
	boltMetaSize       = 64
	// boltPgidNoFreelist is the meta.freelist value bbolt writes under
	// NoFreelistSync: "rebuild the freelist from reachability on open".
	boltPgidNoFreelist = 0xffffffffffffffff
	// boltMaxPageSize bounds how much of the file the header read needs: both
	// metas sit inside the first two pages.
	boltMaxPageSize = 64 << 10
)

// pieceCompletionNeedsCheck reports whether the file at path can carry the
// damage class the checker exists for, with the reason.
//
// The "page N already freed" panic needs a PERSISTED freelist that disagrees
// with the tree, and bbolt only loads a persisted freelist when the meta points
// at one. A file our backend has committed to carries boltPgidNoFreelist in
// its live meta (bbolt stamps it on every commit under NoFreelistSync) and is
// rebuilt from reachability at open, so for those the check is pure cost — a
// re-exec'd child, a full tree walk — on every boot forever. So: run it when
// either meta still names a freelist page (a 1.11.x file, checked once, then
// migrated by our first commit), or when the header does not even parse (the
// child will say what is wrong). Both metas are looked at because bbolt picks
// the higher-txid one that validates, which this does not re-implement.
func pieceCompletionNeedsCheck(path string) (bool, string) {
	f, err := os.Open(path)
	if err != nil {
		return true, "header unreadable: " + err.Error()
	}
	defer f.Close()
	raw := make([]byte, 2*boltMaxPageSize)
	n, err := io.ReadFull(f, raw)
	if err != nil && err != io.ErrUnexpectedEOF {
		return true, "header unreadable: " + err.Error()
	}
	return boltHeaderNeedsCheck(raw[:n])
}

// boltHeaderNeedsCheck is the pure half of pieceCompletionNeedsCheck.
func boltHeaderNeedsCheck(raw []byte) (bool, string) {
	le := binary.LittleEndian
	if len(raw) < boltPageHeaderSize+boltMetaSize {
		return true, "file shorter than one meta page"
	}
	m0 := raw[boltPageHeaderSize:]
	if le.Uint32(m0) != boltMagic {
		return true, "meta0 magic mismatch"
	}
	pageSize := int(le.Uint32(m0[8:]))
	if pageSize < boltPageHeaderSize+boltMetaSize || pageSize > boltMaxPageSize {
		return true, "meta0 page size out of range"
	}
	for i := 0; i < 2; i++ {
		off := i*pageSize + boltPageHeaderSize
		if off+boltMetaSize > len(raw) {
			return true, "file shorter than two meta pages"
		}
		m := raw[off:]
		if le.Uint32(m) != boltMagic {
			return true, "meta magic mismatch"
		}
		if le.Uint64(m[32:]) != boltPgidNoFreelist {
			return true, "persisted freelist (file written by the library backend)"
		}
	}
	return false, "both metas carry no persisted freelist"
}

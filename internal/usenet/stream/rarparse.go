package stream

import "encoding/binary"

// rarChunk is one contiguous run of a stored file's bytes living inside ONE RAR
// volume. A single-volume archive yields one chunk per file; a file split across
// N volumes yields N chunks (one per volume) that reassemble, in volume order,
// into the whole file. Because the streaming path only ever accepts the STORE
// method, chunk data is the file's bytes verbatim — no decompression — so a chunk
// maps a file-byte range directly onto a byte range inside the volume container.
type rarChunk struct {
	name       string // file name recorded in the RAR file header
	volIndex   int    // index of the volume this chunk lives in (caller-assigned)
	dataOffset int64  // byte offset of the chunk's data within the volume container
	packSize   int64  // number of file bytes stored in this volume (data length)
	unpSize    int64  // total unpacked size of the whole file (repeated per volume)
	stored     bool   // true when the compression method is STORE (0% compression)
	encrypted  bool   // true when the file/header is password-encrypted
}

// rar4Marker is the fixed RAR 1.5–4.x signature (7 bytes).
var rar4Marker = []byte{0x52, 0x61, 0x72, 0x21, 0x1a, 0x07, 0x00}

// rar5Marker is the RAR 5.0 signature (8 bytes): the first six bytes match RAR4,
// the seventh distinguishes the format (0x01 vs 0x00) and an eighth 0x00 follows.
var rar5Marker = []byte{0x52, 0x61, 0x72, 0x21, 0x1a, 0x07, 0x01, 0x00}

// parseVolume reads a volume's signature and dispatches to the RAR4 or RAR5
// header walker, returning the file chunks it contains. volIndex is stamped onto
// every returned chunk so the aggregator can order split files across volumes. A
// signature it does not recognise is reported as NotStreamable so the caller
// falls back to the batch download rather than guessing.
func parseVolume(vs volumeSource, volIndex int) ([]rarChunk, error) {
	sig, err := vs.readAt(0, int64(len(rar5Marker)))
	if err != nil {
		return nil, notStreamable("read rar signature: " + err.Error())
	}
	switch {
	case bytesEqual(sig, rar5Marker):
		return parseRar5(vs, volIndex)
	case bytesEqual(sig[:len(rar4Marker)], rar4Marker):
		return parseRar4(vs, volIndex)
	default:
		return nil, notStreamable("unrecognised RAR signature")
	}
}

// bytesEqual reports byte-slice equality without importing bytes for one call.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// u16 reads a little-endian uint16 at off, reporting ok=false when out of range.
func u16(b []byte, off int) (uint16, bool) {
	if off < 0 || off+2 > len(b) {
		return 0, false
	}
	return binary.LittleEndian.Uint16(b[off : off+2]), true
}

// u32 reads a little-endian uint32 at off, reporting ok=false when out of range.
func u32(b []byte, off int) (uint32, bool) {
	if off < 0 || off+4 > len(b) {
		return 0, false
	}
	return binary.LittleEndian.Uint32(b[off : off+4]), true
}

// readVint decodes a RAR5 variable-length integer at off: little-endian base-128
// with the high bit as a continuation flag. It returns the value, the number of
// bytes consumed, and ok=false on truncation or an over-long (>10 byte) encoding.
func readVint(b []byte, off int) (val uint64, n int, ok bool) {
	var shift uint
	for i := 0; i < 10; i++ {
		if off+i >= len(b) {
			return 0, 0, false
		}
		c := b[off+i]
		val |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			return val, i + 1, true
		}
		shift += 7
	}
	return 0, 0, false
}

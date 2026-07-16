package nntptest

import (
	"encoding/binary"
	"hash/crc32"
)

// RAR 5.0 fixture builder. It emits spec-shaped RAR5 volumes that STORE a video
// file (compression method 0) split across volumes, mirroring buildRar4Store but
// for the newer format so the streaming classifier's RAR5 parser can be tested
// hermetically. Blocks are: HeadCRC(uint32) + vint(HeaderSize) + header; file
// data (DataSize bytes) follows the header verbatim (store = 0% compression).

// rar5Sig is the RAR 5.0 signature (8 bytes).
var rar5Sig = []byte{0x52, 0x61, 0x72, 0x21, 0x1a, 0x07, 0x01, 0x00}

const (
	rar5HTypeMain = 1 // main archive header
	rar5HTypeFile = 2 // file header
	rar5HTypeEnd  = 5 // end-of-archive header

	rar5HFlagData      = 0x0002 // header followed by DataSize bytes
	rar5HFlagSplitBef  = 0x0008 // data continues a file from the previous volume
	rar5HFlagSplitAft  = 0x0010 // data continues into the next volume
	rar5ArchiveVolume  = 0x0001 // archive is part of a multi-volume set
	rar5CompStore      = 0x0000 // CompressionInfo with method bits 7-9 == 0 (store)
	rar5HostOSUnix     = 1
	rar5FileFlagsPlain = 0x0000 // no mtime, no data-CRC, unpacked size known
)

// buildRar5Store builds a multi-volume RAR5 archive STOREing videoName, splitting
// its bytes across volumes of at most volSize each. It returns one raw container
// byte slice per volume in assembly order — the .part01.rar, .part02.rar, … the
// caller posts as NZB files.
func buildRar5Store(videoName string, content []byte, volSize int) [][]byte {
	if volSize <= 0 || volSize > len(content) {
		volSize = len(content)
	}
	if volSize <= 0 {
		volSize = 1
	}
	total := uint64(len(content))

	var volumes [][]byte
	for off := 0; off < len(content) || off == 0 && len(content) == 0; off += volSize {
		end := off + volSize
		if end > len(content) {
			end = len(content)
		}
		vol := rar5Volume(rar5VolParams{
			name:      rarStoredName(videoName),
			chunk:     content[off:end],
			total:     total,
			hasBefore: off > 0,
			hasAfter:  end < len(content),
		})
		volumes = append(volumes, vol)
		if end >= len(content) {
			break
		}
	}
	return volumes
}

type rar5VolParams struct {
	name      string
	chunk     []byte
	total     uint64
	hasBefore bool
	hasAfter  bool
}

// rar5Volume assembles one RAR5 volume: signature, main header, file header + its
// stored data chunk, and (on the final volume) an end-of-archive header.
func rar5Volume(p rar5VolParams) []byte {
	var b []byte
	b = append(b, rar5Sig...)
	b = append(b, rar5MainBlock()...)
	b = append(b, rar5FileBlock(p)...)
	if !p.hasAfter {
		b = append(b, rar5EndBlock()...)
	}
	return b
}

// rar5MainBlock builds the main archive header marking a multi-volume archive.
func rar5MainBlock() []byte {
	body := rar5Body(rar5HTypeMain, 0, encodeVint(rar5ArchiveVolume))
	return rar5Frame(body)
}

// rar5FileBlock builds a file header for one volume's chunk plus the chunk data.
func rar5FileBlock(p rar5VolParams) []byte {
	flags := uint64(rar5HFlagData)
	if p.hasBefore {
		flags |= rar5HFlagSplitBef
	}
	if p.hasAfter {
		flags |= rar5HFlagSplitAft
	}

	var spec []byte
	spec = append(spec, encodeVint(rar5FileFlagsPlain)...)
	spec = append(spec, encodeVint(p.total)...)       // UnpackedSize (whole file)
	spec = append(spec, encodeVint(0)...)             // Attributes
	spec = append(spec, encodeVint(rar5CompStore)...) // CompressionInfo (store)
	spec = append(spec, encodeVint(rar5HostOSUnix)...)
	spec = append(spec, encodeVint(uint64(len(p.name)))...)
	spec = append(spec, []byte(p.name)...)

	body := rar5BodyWithData(rar5HTypeFile, flags, uint64(len(p.chunk)), spec)
	block := rar5Frame(body)
	return append(block, p.chunk...)
}

// rar5EndBlock builds the end-of-archive header closing the last volume.
func rar5EndBlock() []byte {
	body := rar5Body(rar5HTypeEnd, 0, encodeVint(0))
	return rar5Frame(body)
}

// rar5Body assembles a header body with no trailing data-size field (flags
// without rar5HFlagData). The store fixtures never emit an extra area.
func rar5Body(htype, flags uint64, typeSpecific []byte) []byte {
	return rar5AssembleBody(htype, flags, 0, false, typeSpecific)
}

// rar5BodyWithData assembles a header body that declares dataSize trailing data.
func rar5BodyWithData(htype, flags, dataSize uint64, typeSpecific []byte) []byte {
	return rar5AssembleBody(htype, flags, dataSize, true, typeSpecific)
}

// rar5AssembleBody builds the header body: type, flags, optional data-size, then
// the type-specific fields. No extra area is emitted (the fixtures do not need
// one), so the classifier's RAR5 file blocks parse as unencrypted.
func rar5AssembleBody(htype, flags, dataSize uint64, withData bool, typeSpecific []byte) []byte {
	var body []byte
	body = append(body, encodeVint(htype)...)
	body = append(body, encodeVint(flags)...)
	if withData {
		body = append(body, encodeVint(dataSize)...)
	}
	body = append(body, typeSpecific...)
	return body
}

// rar5Frame prepends the HeadCRC (CRC32 over HeaderSize vint + body) and the
// HeaderSize vint, producing a complete block header ready to be followed by any
// data area.
func rar5Frame(body []byte) []byte {
	hs := encodeVint(uint64(len(body)))
	crcInput := append(append([]byte(nil), hs...), body...)
	crc := crc32.ChecksumIEEE(crcInput)

	out := make([]byte, 4)
	binary.LittleEndian.PutUint32(out, crc)
	out = append(out, hs...)
	out = append(out, body...)
	return out
}

// encodeVint encodes v as a RAR5 variable-length integer (little-endian base-128,
// high bit = continuation).
func encodeVint(v uint64) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			out = append(out, b|0x80)
			continue
		}
		out = append(out, b)
		return out
	}
}

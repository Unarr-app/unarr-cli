package stream

// RAR 1.5–4.x block parsing. Only what the streaming classifier needs is
// decoded: enough of the main and file headers to learn each file's compression
// method, encryption state, name and data location. Data is never read here —
// PACK_SIZE is used purely as arithmetic to skip to the next block.

const (
	rar4TypeMain = 0x73 // MAIN_HEAD
	rar4TypeFile = 0x74 // FILE_HEAD
	rar4TypeEnd  = 0x7b // ENDARC_HEAD
)

const (
	rar4MainEncrypted = 0x0080 // MHD_ENCRYPTVER-ish: headers encrypted
	rar4FileLongBlock = 0x8000 // LHD_LONG_BLOCK: ADD_SIZE (PACK_SIZE) follows header
	rar4FilePassword  = 0x0004 // LHD_PASSWORD: file data encrypted
	rar4FileLarge     = 0x0100 // LHD_LARGE: 64-bit HIGH_PACK/HIGH_UNP sizes present
)

const (
	rar4StoreMethod = 0x30 // method 0x30 == stored (no compression)
	rar4BaseHdrLen  = 7    // HEAD_CRC(2)+TYPE(1)+FLAGS(2)+HEAD_SIZE(2)
	rar4MaxBlocks   = 100_000
)

// parseRar4 walks the block chain of a RAR4 volume (after its 7-byte marker) and
// returns one rarChunk per file block. It advances block-by-block using each
// block's HEAD_SIZE plus (for file blocks) its PACK_SIZE, bounded by the volume
// size so a corrupt length can never loop or read forever.
func parseRar4(vs volumeSource, volIndex int) ([]rarChunk, error) {
	var chunks []rarChunk
	pos := int64(len(rar4Marker))
	size := vs.size()

	for blocks := 0; blocks < rar4MaxBlocks && pos < size; blocks++ {
		base, err := vs.readAt(pos, rar4BaseHdrLen)
		if err != nil {
			return nil, notStreamable("rar4 read block header: " + err.Error())
		}
		blockType := base[2]
		flags, _ := u16(base, 3)
		headSize, _ := u16(base, 5)
		if headSize < rar4BaseHdrLen {
			return nil, notStreamable("rar4 bad header size")
		}
		hdr, err := vs.readAt(pos, int64(headSize))
		if err != nil {
			return nil, notStreamable("rar4 read full header: " + err.Error())
		}

		dataSize, chunk, err := rar4Block(blockType, flags, hdr, pos, volIndex)
		if err != nil {
			return nil, err
		}
		if chunk != nil {
			chunks = append(chunks, *chunk)
		}
		if blockType == rar4TypeEnd {
			break
		}
		pos += int64(headSize) + dataSize
	}
	return chunks, nil
}

// rar4Block interprets one block header. It returns the block's trailing data
// size (to advance past), and a file chunk when the block is a file header. A
// main header with encrypted headers is rejected outright.
func rar4Block(blockType byte, flags uint16, hdr []byte, pos int64, volIndex int) (int64, *rarChunk, error) {
	switch blockType {
	case rar4TypeMain:
		if flags&rar4MainEncrypted != 0 {
			return 0, nil, notStreamable("rar4 encrypted headers")
		}
		return 0, nil, nil
	case rar4TypeFile:
		return rar4FileBlock(flags, hdr, pos, volIndex)
	default:
		// Any other block (comment, sub, end): ADD_SIZE only when LONG_BLOCK set.
		return rar4AddSize(flags, hdr), nil, nil
	}
}

// rar4AddSize returns the ADD_SIZE (extra data after the header) of a non-file
// block: PACK_SIZE at offset 7 when LONG_BLOCK is set, else zero.
func rar4AddSize(flags uint16, hdr []byte) int64 {
	if flags&rar4FileLongBlock == 0 {
		return 0
	}
	v, _ := u32(hdr, 7)
	return int64(v)
}

// rar4FileBlock decodes a FILE_HEAD into a rarChunk. PACK_SIZE (this volume's
// data length) sits at offset 7; UNP_SIZE (whole-file size) at 11; METHOD at 25;
// NAME_SIZE at 26; NAME at 32 (+8 when the LARGE flag adds 64-bit high words).
func rar4FileBlock(flags uint16, hdr []byte, pos int64, volIndex int) (int64, *rarChunk, error) {
	const minFileHdr = 32
	if len(hdr) < minFileHdr {
		return 0, nil, notStreamable("rar4 truncated file header")
	}
	packLo, _ := u32(hdr, 7)
	unpLo, _ := u32(hdr, 11)
	method := hdr[25]
	nameSize, _ := u16(hdr, 26)

	packSize := int64(packLo)
	unpSize := int64(unpLo)
	nameOff := minFileHdr
	if flags&rar4FileLarge != 0 {
		highPack, _ := u32(hdr, 32)
		highUnp, _ := u32(hdr, 36)
		packSize |= int64(highPack) << 32
		unpSize |= int64(highUnp) << 32
		nameOff = 40
	}
	if nameOff+int(nameSize) > len(hdr) {
		return 0, nil, notStreamable("rar4 name runs past header")
	}
	name := string(hdr[nameOff : nameOff+int(nameSize)])

	chunk := rarChunk{
		name:       rarBaseName(name),
		volIndex:   volIndex,
		dataOffset: pos + int64(len(hdr)),
		packSize:   packSize,
		unpSize:    unpSize,
		stored:     method == rar4StoreMethod,
		encrypted:  flags&rar4FilePassword != 0,
	}
	return packSize, &chunk, nil
}

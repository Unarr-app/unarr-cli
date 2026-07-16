package stream

// RAR 5.0 block parsing. Blocks are: HeadCRC(uint32) + vint(HeaderSize) + header
// bytes; file/service data (DataSize bytes) follows the header. Header fields are
// vints. As with RAR4 only the classifier's needs are decoded — method,
// encryption, name, size and data location — and data is skipped arithmetically.

const (
	rar5TypeFile = 2 // FILE header
	rar5TypeEnd  = 5 // ENDARC header
)

const (
	rar5HdrFlagExtra = 0x0001 // header carries an extra area
	rar5HdrFlagData  = 0x0002 // header is followed by DataSize data bytes
)

const (
	rar5FileFlagMtime  = 0x0002 // mtime (uint32) present
	rar5FileFlagCRC    = 0x0004 // data CRC32 (uint32) present
	rar5FileFlagUnpUnk = 0x0008 // unpacked size unknown
)

const (
	rar5MethodMask   = 0x0380 // bits 7-9 of CompressionInfo hold the method
	rar5MethodShift  = 7
	rar5ExtraCrypt   = 1 // extra-area record type: file encryption
	rar5CRCLen       = 4
	rar5MaxBlocks    = 100_000
	rar5VintProbeLen = 16 // bytes read to decode the leading HeaderSize vint
)

// parseRar5 walks the block chain of a RAR5 volume (after its 8-byte marker),
// returning one rarChunk per file block. It bounds the walk by the volume size so
// a malformed length cannot loop.
func parseRar5(vs volumeSource, volIndex int) ([]rarChunk, error) {
	var chunks []rarChunk
	pos := int64(len(rar5Marker))
	size := vs.size()

	for blocks := 0; blocks < rar5MaxBlocks && pos < size; blocks++ {
		nextPos, chunk, next, err := rar5Block(vs, pos, volIndex)
		if err != nil {
			return nil, err
		}
		if chunk != nil {
			chunks = append(chunks, *chunk)
		}
		if next == rar5Stop {
			break
		}
		pos = nextPos
	}
	return chunks, nil
}

// rar5 walk sentinels.
const (
	rar5Continue = 0
	rar5Stop     = 1
)

// rar5Block reads the block at pos and returns the absolute position of the next
// block, the file chunk (nil for non-file blocks), and a stop sentinel for the
// end-of-archive block.
func rar5Block(vs volumeSource, pos int64, volIndex int) (nextPos int64, chunk *rarChunk, stop int, err error) {
	// Read only what remains for the HeaderSize vint: the final (end-of-archive)
	// block is smaller than the probe window, so a fixed read would run off the end.
	avail := vs.size() - (pos + rar5CRCLen)
	if avail <= 0 {
		return 0, nil, rar5Stop, notStreamable("rar5 truncated at block header")
	}
	probeLen := int64(rar5VintProbeLen)
	if avail < probeLen {
		probeLen = avail
	}
	probe, err := vs.readAt(pos+rar5CRCLen, probeLen)
	if err != nil {
		return 0, nil, rar5Stop, notStreamable("rar5 read header size: " + err.Error())
	}
	hsize, k, ok := readVint(probe, 0)
	if !ok || hsize == 0 {
		return 0, nil, rar5Stop, notStreamable("rar5 bad header size vint")
	}
	hdrStart := pos + rar5CRCLen + int64(k)
	hdr, err := vs.readAt(hdrStart, int64(hsize))
	if err != nil {
		return 0, nil, rar5Stop, notStreamable("rar5 read header: " + err.Error())
	}

	h, err := parseRar5Header(hdr)
	if err != nil {
		return 0, nil, rar5Stop, err
	}
	dataStart := hdrStart + int64(hsize)
	if h.blockType == rar5TypeEnd {
		return 0, nil, rar5Stop, nil
	}
	if h.blockType == rar5TypeFile {
		chunk = rar5FileChunk(h, dataStart, volIndex)
	}
	return dataStart + h.dataSize, chunk, rar5Continue, nil
}

// rar5Header holds the decoded fields of one RAR5 block header the classifier
// cares about.
type rar5Header struct {
	blockType uint64
	dataSize  int64
	// file-only fields (valid when blockType == rar5TypeFile)
	name      string
	unpSize   int64
	stored    bool
	encrypted bool
}

// parseRar5Header decodes the common header prefix (type, flags, optional extra
// and data sizes) and, for a file block, the file-specific fields.
func parseRar5Header(hdr []byte) (rar5Header, error) {
	var h rar5Header
	p := 0
	blockType, n, ok := readVint(hdr, p)
	if !ok {
		return h, notStreamable("rar5 bad block type")
	}
	p += n
	flags, n, ok := readVint(hdr, p)
	if !ok {
		return h, notStreamable("rar5 bad header flags")
	}
	p += n

	var extraSize uint64
	if flags&rar5HdrFlagExtra != 0 {
		if extraSize, n, ok = readVint(hdr, p); !ok {
			return h, notStreamable("rar5 bad extra size")
		}
		p += n
	}
	if flags&rar5HdrFlagData != 0 {
		var ds uint64
		if ds, n, ok = readVint(hdr, p); !ok {
			return h, notStreamable("rar5 bad data size")
		}
		p += n
		h.dataSize = int64(ds)
	}
	h.blockType = blockType
	if blockType != rar5TypeFile {
		return h, nil
	}
	return parseRar5File(h, hdr, p, int(extraSize))
}

// parseRar5File decodes the file-specific header fields starting at p and, from
// the trailing extra area, whether the file is encrypted.
func parseRar5File(h rar5Header, hdr []byte, p, extraSize int) (rar5Header, error) {
	fileFlags, n, ok := readVint(hdr, p)
	if !ok {
		return h, notStreamable("rar5 bad file flags")
	}
	p += n
	unpSize, n, ok := readVint(hdr, p)
	if !ok {
		return h, notStreamable("rar5 bad unpacked size")
	}
	p += n
	if fileFlags&rar5FileFlagUnpUnk != 0 {
		return h, notStreamable("rar5 unknown unpacked size")
	}
	if _, n, ok = readVint(hdr, p); !ok { // attributes
		return h, notStreamable("rar5 bad attributes")
	}
	p += n
	if fileFlags&rar5FileFlagMtime != 0 {
		p += rar5CRCLen
	}
	if fileFlags&rar5FileFlagCRC != 0 {
		p += rar5CRCLen
	}
	compInfo, n, ok := readVint(hdr, p)
	if !ok {
		return h, notStreamable("rar5 bad compression info")
	}
	p += n
	name, err := rar5FileName(hdr, p)
	if err != nil {
		return h, err
	}
	h.name = name
	h.unpSize = int64(unpSize)
	h.stored = (compInfo&rar5MethodMask)>>rar5MethodShift == 0
	h.encrypted = rar5HasCrypt(hdr, extraSize)
	return h, nil
}

// rar5FileName reads HostOS + NameLength + Name starting at p.
func rar5FileName(hdr []byte, p int) (string, error) {
	_, n, ok := readVint(hdr, p) // host OS
	if !ok {
		return "", notStreamable("rar5 bad host os")
	}
	p += n
	nameLen, n, ok := readVint(hdr, p)
	if !ok {
		return "", notStreamable("rar5 bad name length")
	}
	p += n
	if p+int(nameLen) > len(hdr) {
		return "", notStreamable("rar5 name runs past header")
	}
	return rarBaseName(string(hdr[p : p+int(nameLen)])), nil
}

// rar5FileChunk builds a rarChunk from a decoded file header positioned at
// dataStart within the volume.
func rar5FileChunk(h rar5Header, dataStart int64, volIndex int) *rarChunk {
	return &rarChunk{
		name:       h.name,
		volIndex:   volIndex,
		dataOffset: dataStart,
		packSize:   h.dataSize,
		unpSize:    h.unpSize,
		stored:     h.stored,
		encrypted:  h.encrypted,
	}
}

// rar5HasCrypt scans the trailing extra area (the last extraSize bytes of the
// header) for a file-encryption record. A parse failure is treated as encrypted
// (conservative: reject rather than stream a possibly-encrypted file).
func rar5HasCrypt(hdr []byte, extraSize int) bool {
	if extraSize <= 0 {
		return false
	}
	if extraSize > len(hdr) {
		return true
	}
	area := hdr[len(hdr)-extraSize:]
	for p := 0; p < len(area); {
		recSize, n, ok := readVint(area, p)
		if !ok || recSize == 0 {
			return true
		}
		recStart := p + n
		if recStart+int(recSize) > len(area) {
			return true
		}
		recType, _, ok := readVint(area, recStart)
		if !ok {
			return true
		}
		if recType == rar5ExtraCrypt {
			return true
		}
		p = recStart + int(recSize)
	}
	return false
}

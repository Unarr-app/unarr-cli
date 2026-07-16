package nntptest

import (
	"encoding/binary"
	"hash/crc32"
	"path/filepath"
	"strings"
)

// rar4 block header types.
const (
	rar4Main = 0x73 // archive (main) header
	rar4File = 0x74 // file header
	rar4End  = 0x7b // end-of-archive header
)

// rar4 header flags.
const (
	rarMainVolume    = 0x0001 // MHD_VOLUME: part of a multi-volume set
	rarMainFirstVol  = 0x0100 // MHD_FIRSTVOLUME: this is volume 1
	rarFileSplitBef  = 0x0001 // LHD_SPLIT_BEFORE: file continued from prev volume
	rarFileSplitAft  = 0x0002 // LHD_SPLIT_AFTER: file continues in next volume
	rarFilePassword  = 0x0004 // LHD_PASSWORD: file data is encrypted
	rarFileLongBlock = 0x8000 // LHD_LONG_BLOCK: PACK_SIZE data follows the header
)

const (
	rarStoreMethod = 0x30       // method 0x30 = stored (0% compression)
	rarUnpVersion  = 20         // version needed to unpack (2.0)
	rarHostUnix    = 3          // HOST_OS = Unix
	rarDOSTime     = 0x50210000 // 2020-01-01 00:00:00 in DOS datetime
	rarFileAttr    = 0x00000020 // archive attribute bit
)

// rar4Marker is the fixed RAR 1.5–4.x signature block.
var rar4Marker = []byte{0x52, 0x61, 0x72, 0x21, 0x1a, 0x07, 0x00}

// buildRar4Store builds a real, unrar-readable RAR4 archive that STOREs
// videoName (method 0, no compression) and splits its bytes across volumes of
// at most volSize each. It returns one byte slice per volume, in order; volume
// 0 is the ".rar", the rest are ".r00", ".r01", …
//
// Because the method is store, the video bytes appear verbatim inside the
// concatenated volumes — exactly the invariant the streaming RAR-store reader
// (C5) will exploit to translate a video offset to a container offset.
func buildRar4Store(videoName string, content []byte, volSize int) [][]byte {
	return buildRar4(videoName, content, volSize, rarStoreMethod, false)
}

// buildRar4 is the parameterised RAR4 builder behind the store fixture and its
// compressed/encrypted rejection variants. method is the RAR4 method byte
// (0x30 = store, 0x31–0x35 = compression levels); encrypted sets the file
// header's password flag. Only the store, unencrypted combination is meant to be
// streamable — the others exist so tests can prove the classifier rejects them.
func buildRar4(videoName string, content []byte, volSize int, method byte, encrypted bool) [][]byte {
	if volSize <= 0 || volSize > len(content) {
		volSize = len(content)
	}
	if volSize <= 0 {
		volSize = 1
	}
	total := int64(len(content))
	fileCRC := crc32.ChecksumIEEE(content)

	var volumes [][]byte
	for off := 0; off < len(content) || off == 0 && len(content) == 0; off += volSize {
		end := off + volSize
		if end > len(content) {
			end = len(content)
		}
		chunk := content[off:end]
		vol := rar4Volume(volumeParams{
			videoName:  videoName,
			chunk:      chunk,
			total:      total,
			fileCRC:    fileCRC,
			isFirstVol: off == 0,
			hasBefore:  off > 0,
			hasAfter:   end < len(content),
			method:     method,
			encrypted:  encrypted,
		})
		volumes = append(volumes, vol)
		if end >= len(content) {
			break
		}
	}
	return volumes
}

type volumeParams struct {
	videoName  string
	chunk      []byte
	total      int64
	fileCRC    uint32
	isFirstVol bool
	hasBefore  bool
	hasAfter   bool
	method     byte
	encrypted  bool
}

// rar4Volume assembles a single volume: marker, main header, file header + its
// data chunk, and (on the final volume) an end-of-archive header.
func rar4Volume(p volumeParams) []byte {
	var b []byte
	b = append(b, rar4Marker...)
	b = append(b, rar4MainHeader(p.isFirstVol)...)
	b = append(b, rar4FileHeader(p)...)
	b = append(b, p.chunk...)
	if !p.hasAfter {
		b = append(b, rar4EndHeader()...)
	}
	return b
}

// rar4MainHeader builds the 13-byte MAIN_HEAD marking a multi-volume archive.
func rar4MainHeader(isFirstVol bool) []byte {
	flags := rarMainVolume
	if isFirstVol {
		flags |= rarMainFirstVol
	}
	h := make([]byte, 13)
	h[2] = rar4Main
	binary.LittleEndian.PutUint16(h[3:5], uint16(flags))
	binary.LittleEndian.PutUint16(h[5:7], 13)
	// h[7:13] reserved (HighPosAV + PosAV) left zero.
	finalizeHeaderCRC(h)
	return h
}

// rar4FileHeader builds the FILE_HEAD for videoName in one volume. PACK_SIZE is
// this volume's chunk length; UNP_SIZE and FILE_CRC describe the whole file.
func rar4FileHeader(p volumeParams) []byte {
	name := []byte(rarStoredName(p.videoName))
	flags := rarFileLongBlock
	if p.hasBefore {
		flags |= rarFileSplitBef
	}
	if p.hasAfter {
		flags |= rarFileSplitAft
	}
	if p.encrypted {
		flags |= rarFilePassword
	}
	method := p.method
	if method == 0 {
		method = rarStoreMethod
	}

	size := 32 + len(name)
	h := make([]byte, size)
	h[2] = rar4File
	binary.LittleEndian.PutUint16(h[3:5], uint16(flags))
	binary.LittleEndian.PutUint16(h[5:7], uint16(size))
	binary.LittleEndian.PutUint32(h[7:11], uint32(len(p.chunk))) // PACK_SIZE
	binary.LittleEndian.PutUint32(h[11:15], uint32(p.total))     // UNP_SIZE
	h[15] = rarHostUnix
	binary.LittleEndian.PutUint32(h[16:20], p.fileCRC)
	binary.LittleEndian.PutUint32(h[20:24], rarDOSTime)
	h[24] = rarUnpVersion
	h[25] = method
	binary.LittleEndian.PutUint16(h[26:28], uint16(len(name)))
	binary.LittleEndian.PutUint32(h[28:32], rarFileAttr)
	copy(h[32:], name)
	finalizeHeaderCRC(h)
	return h
}

// rar4EndHeader builds the 7-byte ENDARC block that closes the last volume.
func rar4EndHeader() []byte {
	h := make([]byte, 7)
	h[2] = rar4End
	binary.LittleEndian.PutUint16(h[5:7], 7)
	finalizeHeaderCRC(h)
	return h
}

// finalizeHeaderCRC writes the RAR4 HEAD_CRC (low 16 bits of the CRC32 of the
// header bytes after the CRC field) into h[0:2].
func finalizeHeaderCRC(h []byte) {
	crc := crc32.ChecksumIEEE(h[2:])
	binary.LittleEndian.PutUint16(h[0:2], uint16(crc&0xffff))
}

// rarStoredName is the path recorded inside the archive: a forward-slash path
// with no directory, matching how single-file releases are stored.
func rarStoredName(videoName string) string {
	return filepath.Base(strings.ReplaceAll(videoName, "\\", "/"))
}

// rarVolumeName returns the on-disk / NZB name of volume i using the classic
// ".rar/.r00/.r01" numbering (no new-style .partNN.rar).
func rarVolumeName(archiveBase string, i int) string {
	if i == 0 {
		return archiveBase + ".rar"
	}
	return archiveBase + rarExtForIndex(i-1)
}

// rarExtForIndex maps 0->".r00", 1->".r01", … 99->".r99", 100->".r100".
func rarExtForIndex(n int) string {
	if n < 100 {
		return "." + "r" + twoDigit(n)
	}
	return ".r" + itoa(n)
}

func twoDigit(n int) string {
	s := itoa(n)
	if len(s) < 2 {
		return "0" + s
	}
	return s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf []byte
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}

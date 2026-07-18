package yenc

import (
	"bytes"
	"fmt"
	"hash/crc32"
)

// defaultLineLength is the column width used when wrapping encoded output.
// yEnc decoders ignore line length entirely (it is purely cosmetic), but real
// posts wrap at 128, so the fixtures mirror that.
const defaultLineLength = 128

// Encode produces the raw yEnc article body for one part of a file — the exact
// inverse of Decode. It is the encoder the download path never needed but the
// tests do: it lets fixtures build deterministic articles that round-trip
// through Decode (CRC32 verified) with no real Usenet server.
//
//   - name      filename advertised in =ybegin (must be last on that line).
//   - partNum   1-based part number; total the number of parts in the file.
//   - begin/end 1-based byte offsets of this part within the file (=ypart),
//     inclusive on both ends, so len(data) == end-begin+1.
//   - fileSize  total size of the whole file (=ybegin size=).
//   - data      the raw bytes of THIS part.
//
// When total <= 1 a single-part body (no =ypart, crc32= in =yend) is emitted;
// otherwise a multipart body (=ypart + pcrc32= in =yend) matching what the
// streaming OffsetIndex relies on.
func Encode(name string, partNum, total int, begin, end, fileSize int64, data []byte) []byte {
	var buf bytes.Buffer
	multipart := total > 1

	if multipart {
		fmt.Fprintf(&buf, "=ybegin part=%d total=%d line=%d size=%d name=%s\r\n",
			partNum, total, defaultLineLength, fileSize, name)
		fmt.Fprintf(&buf, "=ypart begin=%d end=%d\r\n", begin, end)
	} else {
		fmt.Fprintf(&buf, "=ybegin line=%d size=%d name=%s\r\n",
			defaultLineLength, fileSize, name)
	}

	writeEncodedData(&buf, data, defaultLineLength)

	crc := crc32.ChecksumIEEE(data)
	if multipart {
		fmt.Fprintf(&buf, "=yend size=%d part=%d pcrc32=%08x\r\n", len(data), partNum, crc)
	} else {
		fmt.Fprintf(&buf, "=yend size=%d crc32=%08x\r\n", len(data), crc)
	}

	return buf.Bytes()
}

// writeEncodedData yEnc-encodes data into buf, wrapping lines near lineLen
// columns. An escape sequence (=X) is never split across a line boundary, so
// the decoder always sees the '=' and its escaped byte together.
func writeEncodedData(buf *bytes.Buffer, data []byte, lineLen int) {
	col := 0
	var seq [2]byte
	for _, b := range data {
		n := encodeByte(b, &seq)
		if col+n > lineLen {
			buf.WriteString("\r\n")
			col = 0
		}
		buf.Write(seq[:n])
		col += n
	}
	if col > 0 {
		buf.WriteString("\r\n")
	}
}

// encodeByte writes the yEnc encoding of a single byte into seq and returns how
// many bytes it produced (1 normally, 2 when the critical value is escaped).
// Byte arithmetic wraps mod 256, matching decodeByte's b-42 / b-106 inverse.
func encodeByte(b byte, seq *[2]byte) int {
	e := b + 42
	switch e {
	case 0x00, 0x0A, 0x0D, '=': // NUL, LF, CR, '=' — the yEnc critical set
		seq[0] = '='
		seq[1] = e + 64
		return 2
	default:
		seq[0] = e
		return 1
	}
}

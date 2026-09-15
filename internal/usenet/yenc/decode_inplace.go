package yenc

import (
	"bytes"
	"fmt"
	"hash/crc32"
)

// DecodeInPlace decodes the yEnc article body held in buf, writing the decoded
// bytes over buf itself, and returns the part with Data = buf[:n].
//
// No second buffer is needed because decoding never outruns the input: every
// encoded byte yields at most one decoded byte, and header lines and line breaks
// yield none, so the write position never passes the read position. The part
// therefore keeps buf's whole backing array alive (cap(Part.Data) == cap(buf)),
// and the caller must not use buf afterwards.
//
// It accepts exactly what Decode accepts and returns the same part: lines end at
// '\n' with one trailing '\r' dropped, an escape does not carry across a line
// break, lines before =ybegin are skipped, the line after =ybegin is data unless
// it is =ypart, and the CRC32 is verified when the article carries one. Unlike
// Decode it allocates only the Part and its header strings, which matters on the
// streaming path: Decode's per-line strings and slices cost several times the
// article's size in garbage.
func DecodeInPlace(buf []byte) (*Part, error) {
	part := &Part{}
	lines := lineScanner{buf: buf}
	lines.skipToYBegin(part)
	if part.Name == "" && part.Size == 0 {
		return nil, fmt.Errorf("yenc: no =ybegin header found")
	}
	part.Data = buf[:lines.decodeBody(part)]
	if part.CRC32 != 0 {
		if computed := crc32.ChecksumIEEE(part.Data); computed != part.CRC32 {
			return nil, fmt.Errorf("yenc: CRC32 mismatch: expected %08x, got %08x", part.CRC32, computed)
		}
	}
	normalizeSinglePart(part)
	return part, nil
}

// skipToYBegin consumes lines up to and including =ybegin, parsing it into part.
func (s *lineScanner) skipToYBegin(part *Part) {
	for start, end, ok := s.next(); ok; start, end, ok = s.next() {
		if line := s.buf[start:end]; bytes.HasPrefix(line, []byte("=ybegin ")) {
			parseYBegin(part, string(line))
			return
		}
	}
}

// decodeBody decodes the lines after =ybegin into the buffer, parsing =ypart and
// =yend into part, and returns the decoded length.
func (s *lineScanner) decodeBody(part *Part) int {
	w := 0
	if start, end, ok := s.next(); ok {
		if line := s.buf[start:end]; bytes.HasPrefix(line, []byte("=ypart ")) {
			parseYPart(part, string(line))
		} else {
			w = decodeLineInto(s.buf, w, start, end)
		}
	}
	for start, end, ok := s.next(); ok; start, end, ok = s.next() {
		if line := s.buf[start:end]; bytes.HasPrefix(line, []byte("=yend")) {
			parseYEnd(part, string(line))
			break
		}
		w = decodeLineInto(s.buf, w, start, end)
	}
	return w
}

// decodeLineInto decodes the encoded line buf[start:end] into buf at w and
// returns the new write position. w <= start, so the write never overtakes the
// bytes still to be read.
func decodeLineInto(buf []byte, w, start, end int) int {
	escaped := false
	for _, b := range buf[start:end] {
		switch {
		case escaped:
			buf[w] = b - 106
			w++
			escaped = false
		case b == '=':
			escaped = true
		default:
			buf[w] = b - 42
			w++
		}
	}
	return w
}

// lineScanner splits a buffer into lines the way bufio.ScanLines does, by index,
// so the caller can rewrite bytes it has already passed.
type lineScanner struct {
	buf []byte
	pos int
}

// next returns the bounds of the next line without its "\n" or "\r\n".
func (s *lineScanner) next() (start, end int, ok bool) {
	if s.pos >= len(s.buf) {
		return 0, 0, false
	}
	start = s.pos
	if i := bytes.IndexByte(s.buf[start:], '\n'); i >= 0 {
		end = start + i
		s.pos = end + 1
	} else {
		end = len(s.buf)
		s.pos = end
	}
	if end > start && s.buf[end-1] == '\r' {
		end--
	}
	return start, end, true
}

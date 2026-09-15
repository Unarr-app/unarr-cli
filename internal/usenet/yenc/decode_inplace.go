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
	var d InPlaceDecoder
	return d.Finish(buf)
}

// InPlaceDecoder is DecodeInPlace for a body that is still arriving: Feed decodes
// the lines received so far over the front of the buffer, so the start of the
// article can be used before its end is read, and Finish completes it. Fed in
// any number of steps it returns exactly what DecodeInPlace returns for the whole
// body. The zero value is ready to use.
type InPlaceDecoder struct {
	part  Part
	pos   int // bytes of the body consumed as whole lines
	w     int // decoded bytes written at the front of the buffer
	phase decodePhase
}

type decodePhase int

const (
	seekingBegin decodePhase = iota // lines before =ybegin are skipped
	afterBegin                      // the next line is =ypart or already data
	inData                          // data lines until =yend
	ended                           // =yend seen: the rest is ignored
	noHeader                        // =ybegin names neither the file nor its size
)

// Feed decodes every complete line of body not yet consumed. body is the article
// read so far: each call passes the same bytes as the last, possibly followed by
// more, and possibly moved to a larger array (the decoded front moves with it).
// A trailing line without its '\n' waits for the next call.
func (d *InPlaceDecoder) Feed(body []byte) {
	for d.pos < len(body) {
		i := bytes.IndexByte(body[d.pos:], '\n')
		if i < 0 {
			return
		}
		d.line(body, d.pos, d.pos+i)
		d.pos += i + 1
	}
}

// Progress reports where the article's data sits in the file — 0-based start and
// exclusive end, end 0 when a single-part article gives no size — and how many of
// its bytes are decoded at the front of the body, once the headers that place it
// have been read. Those bytes are final: later lines are written after them.
func (d *InPlaceDecoder) Progress() (start, end int64, decoded int, ok bool) {
	if d.phase != inData && d.phase != ended {
		return 0, 0, 0, false
	}
	if d.part.Begin == 0 && d.part.End == 0 {
		return 0, d.part.Size, d.w, true // single part: normalizeSinglePart's range
	}
	if d.part.Begin < 1 {
		return 0, 0, 0, false
	}
	return d.part.Begin - 1, d.part.End, d.w, true
}

// Finish decodes what Feed has not, including a last line without '\n', and
// returns the part with Data = body[:n], verifying the CRC32.
func (d *InPlaceDecoder) Finish(body []byte) (*Part, error) {
	d.Feed(body)
	if d.pos < len(body) {
		d.line(body, d.pos, len(body))
		d.pos = len(body)
	}
	if d.phase == seekingBegin || d.phase == noHeader {
		return nil, fmt.Errorf("yenc: no =ybegin header found")
	}
	part := d.part
	part.Data = body[:d.w]
	if part.CRC32 != 0 {
		if computed := crc32.ChecksumIEEE(part.Data); computed != part.CRC32 {
			return nil, fmt.Errorf("yenc: CRC32 mismatch: expected %08x, got %08x", part.CRC32, computed)
		}
	}
	normalizeSinglePart(&part)
	return &part, nil
}

// line consumes the line body[start:end] ('\n' excluded).
func (d *InPlaceDecoder) line(body []byte, start, end int) {
	if end > start && body[end-1] == '\r' {
		end--
	}
	line := body[start:end]
	switch d.phase {
	case seekingBegin:
		if bytes.HasPrefix(line, []byte("=ybegin ")) {
			parseYBegin(&d.part, string(line))
			d.phase = afterBegin
			if d.part.Name == "" && d.part.Size == 0 {
				d.phase = noHeader
			}
		}
	case afterBegin:
		d.phase = inData
		if bytes.HasPrefix(line, []byte("=ypart ")) {
			parseYPart(&d.part, string(line))
			return
		}
		d.w = decodeLineInto(body, d.w, start, end)
	case inData:
		if bytes.HasPrefix(line, []byte("=yend")) {
			parseYEnd(&d.part, string(line))
			d.phase = ended
			return
		}
		d.w = decodeLineInto(body, d.w, start, end)
	case ended, noHeader:
	}
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

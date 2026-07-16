package yenc

import (
	"bytes"
	"hash/crc32"
	"testing"
)

// fullByteRange returns a slice covering every possible byte value, including
// the yEnc critical bytes (which force escape sequences on encode).
func fullByteRange() []byte {
	b := make([]byte, 256)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

func TestEncodeDecodeRoundTripMultipart(t *testing.T) {
	data := fullByteRange()
	fileSize := int64(len(data)) * 3
	body := Encode("movie.mkv", 2, 3, int64(len(data))+1, int64(len(data))*2, fileSize, data)

	part, err := DecodeBytes(body)
	if err != nil {
		t.Fatalf("DecodeBytes: %v", err)
	}
	if !bytes.Equal(part.Data, data) {
		t.Fatalf("round-trip mismatch: got %d bytes, want %d", len(part.Data), len(data))
	}
	if part.Name != "movie.mkv" {
		t.Errorf("Name = %q, want movie.mkv", part.Name)
	}
	if part.Number != 2 || part.Total != 3 {
		t.Errorf("Number/Total = %d/%d, want 2/3", part.Number, part.Total)
	}
	if part.Begin != int64(len(data))+1 || part.End != int64(len(data))*2 {
		t.Errorf("Begin/End = %d/%d", part.Begin, part.End)
	}
	if part.Size != fileSize {
		t.Errorf("Size = %d, want %d", part.Size, fileSize)
	}
}

func TestEncodeDecodeRoundTripSinglePart(t *testing.T) {
	data := []byte("a single-part yEnc payload with = and \n and \r bytes")
	body := Encode("readme.txt", 1, 1, 1, int64(len(data)), int64(len(data)), data)

	part, err := DecodeBytes(body)
	if err != nil {
		t.Fatalf("DecodeBytes: %v", err)
	}
	if !bytes.Equal(part.Data, data) {
		t.Fatalf("single-part round-trip mismatch")
	}
	if part.Name != "readme.txt" {
		t.Errorf("Name = %q", part.Name)
	}
	// A single-part article carries no =ypart, but Decode must still synthesize the
	// whole-file byte range it implies, so downstream (streaming OffsetIndex) sees a
	// self-consistent part rather than an empty [0,0) range. Regression guard: a
	// lone-article file (or a small final RAR volume) once decoded as 0 bytes.
	if part.Begin != 1 {
		t.Errorf("single-part Begin = %d, want 1", part.Begin)
	}
	if part.End != int64(len(data)) {
		t.Errorf("single-part End = %d, want %d", part.End, len(data))
	}
	if part.Size != int64(len(data)) {
		t.Errorf("single-part Size = %d, want %d", part.Size, len(data))
	}
}

func TestDecodeSinglePartNoYbeginSizeSynthesizesRange(t *testing.T) {
	// Some single-part posters omit size= from =ybegin entirely. Decode must still
	// derive Begin/End/Size from the decoded data so the part is usable.
	data := []byte("payload-without-declared-size")
	body := Encode("x.bin", 1, 1, 1, int64(len(data)), 0, data)
	part, err := DecodeBytes(body)
	if err != nil {
		t.Fatalf("DecodeBytes: %v", err)
	}
	if part.Begin != 1 || part.End != int64(len(data)) || part.Size != int64(len(data)) {
		t.Fatalf("Begin/End/Size = %d/%d/%d, want 1/%d/%d",
			part.Begin, part.End, part.Size, len(data), len(data))
	}
}

func TestEncodeCRCMatchesDecoder(t *testing.T) {
	data := fullByteRange()
	body := Encode("f.bin", 1, 4, 1, int64(len(data)), int64(len(data))*4, data)

	part, err := DecodeBytes(body)
	if err != nil {
		t.Fatalf("DecodeBytes: %v", err)
	}
	if part.CRC32 != crc32.ChecksumIEEE(data) {
		t.Errorf("CRC32 = %08x, want %08x", part.CRC32, crc32.ChecksumIEEE(data))
	}
}

func TestEncodeCorruptionDetected(t *testing.T) {
	data := []byte("payload that will be tampered with after encoding")
	body := Encode("f.bin", 1, 2, 1, int64(len(data)), int64(len(data))*2, data)

	// Flip a byte in the encoded data region (after the two header lines) so the
	// decoder recomputes a CRC32 that no longer matches pcrc32.
	idx := bytes.IndexByte(body, '\n')
	idx = bytes.IndexByte(body[idx+1:], '\n') + idx + 1 // past =ypart line
	body[idx+1] ^= 0x01

	if _, err := DecodeBytes(body); err == nil {
		t.Fatal("expected CRC32 mismatch error on tampered body, got nil")
	}
}

func TestEncodeLineWrapping(t *testing.T) {
	// 500 zero bytes → 500 encoded chars (each 0x00 encodes to 0x2A '*', not
	// critical). Lines must wrap near 128 and never exceed it.
	data := make([]byte, 500)
	body := Encode("z.bin", 1, 2, 1, int64(len(data)), int64(len(data))*2, data)

	for _, line := range bytes.Split(body, []byte("\r\n")) {
		if len(line) > defaultLineLength {
			t.Fatalf("line length %d exceeds %d: %q", len(line), defaultLineLength, line)
		}
	}
}

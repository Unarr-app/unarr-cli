package yenc

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"strings"
	"testing"
)

func TestDecodeLine(t *testing.T) {
	// yEnc: each byte = (original + 42) % 256
	// So to encode byte 0x00, we store 0x2A (42)
	// To encode byte 0x01, we store 0x2B (43)

	// Encode "Hello" manually:
	// H=72 → 72+42=114='r'
	// e=101 → 101+42=143='\x8f'
	// l=108 → 108+42=150='\x96'
	// l=108 → same
	// o=111 → 111+42=153='\x99'
	input := string([]byte{114, 143, 150, 150, 153})
	decoded := decodeLine(input)
	if string(decoded) != "Hello" {
		t.Errorf("decodeLine: got %q, want %q", string(decoded), "Hello")
	}
}

func TestDecodeLineWithEscape(t *testing.T) {
	// Escaped characters: =\x00 (NUL), =\n (LF), =\r (CR), == (=)
	// Escape: '=' followed by byte, decoded as (byte - 64 - 42) = (byte - 106)

	// To encode byte 0x00 (NUL): escape it → '=' + (0 + 42 + 64) = '=' + 106 = '=' + 'j'
	input := "=j" // should decode to 0x00
	decoded := decodeLine(input)
	if len(decoded) != 1 || decoded[0] != 0x00 {
		t.Errorf("escape decode: got %v, want [0x00]", decoded)
	}
}

func TestDecodeSimpleArticle(t *testing.T) {
	// Create a simple yEnc encoded article
	original := []byte("Hello, World! This is a test of yEnc encoding.")
	encoded := encodeForTest(original)

	crc := crc32.ChecksumIEEE(original)

	article := fmt.Sprintf("=ybegin line=128 size=%d name=test.txt\r\n%s\r\n=yend size=%d crc32=%08x\r\n",
		len(original), encoded, len(original), crc)

	part, err := Decode(strings.NewReader(article))
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if part.Name != "test.txt" {
		t.Errorf("Name: got %q, want %q", part.Name, "test.txt")
	}
	if part.Size != int64(len(original)) {
		t.Errorf("Size: got %d, want %d", part.Size, len(original))
	}
	if !bytes.Equal(part.Data, original) {
		t.Errorf("Data mismatch:\n  got:  %s\n  want: %s", hex.EncodeToString(part.Data), hex.EncodeToString(original))
	}
}

func TestDecodeMultipart(t *testing.T) {
	original := []byte("Part one data here")
	encoded := encodeForTest(original)

	crc := crc32.ChecksumIEEE(original)

	article := fmt.Sprintf("=ybegin part=1 total=3 line=128 size=1000 name=movie.mkv\r\n"+
		"=ypart begin=1 end=%d\r\n"+
		"%s\r\n"+
		"=yend size=%d part=1 pcrc32=%08x\r\n",
		len(original), encoded, len(original), crc)

	part, err := Decode(strings.NewReader(article))
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if part.Number != 1 {
		t.Errorf("Number: got %d, want 1", part.Number)
	}
	if part.Total != 3 {
		t.Errorf("Total: got %d, want 3", part.Total)
	}
	if part.Begin != 1 {
		t.Errorf("Begin: got %d, want 1", part.Begin)
	}
	if part.End != int64(len(original)) {
		t.Errorf("End: got %d, want %d", part.End, len(original))
	}
	if part.Name != "movie.mkv" {
		t.Errorf("Name: got %q", part.Name)
	}
	if !bytes.Equal(part.Data, original) {
		t.Error("Data mismatch")
	}
}

func TestDecodeCRC32Mismatch(t *testing.T) {
	original := []byte("test data")
	encoded := encodeForTest(original)

	article := fmt.Sprintf("=ybegin line=128 size=%d name=test.bin\r\n%s\r\n=yend size=%d crc32=deadbeef\r\n",
		len(original), encoded, len(original))

	_, err := Decode(strings.NewReader(article))
	if err == nil {
		t.Error("expected CRC32 mismatch error")
	}
	if !strings.Contains(err.Error(), "CRC32 mismatch") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDecodeNoHeader(t *testing.T) {
	_, err := Decode(strings.NewReader("just some random data\r\n"))
	if err == nil {
		t.Error("expected error for missing header")
	}
}

func TestDecodeBytes(t *testing.T) {
	original := []byte("quick test")
	encoded := encodeForTest(original)
	crc := crc32.ChecksumIEEE(original)

	article := fmt.Sprintf("=ybegin line=128 size=%d name=q.bin\r\n%s\r\n=yend size=%d crc32=%08x\r\n",
		len(original), encoded, len(original), crc)

	part, err := DecodeBytes([]byte(article))
	if err != nil {
		t.Fatalf("DecodeBytes failed: %v", err)
	}
	if !bytes.Equal(part.Data, original) {
		t.Error("Data mismatch")
	}
}

func TestDecodeBinaryData(t *testing.T) {
	// Test with all byte values 0-255
	original := make([]byte, 256)
	for i := range original {
		original[i] = byte(i)
	}

	encoded := encodeForTest(original)
	crc := crc32.ChecksumIEEE(original)

	article := fmt.Sprintf("=ybegin line=128 size=%d name=binary.bin\r\n%s\r\n=yend size=%d crc32=%08x\r\n",
		len(original), encoded, len(original), crc)

	part, err := Decode(strings.NewReader(article))
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	if !bytes.Equal(part.Data, original) {
		t.Errorf("Binary data mismatch: got %d bytes, want %d", len(part.Data), len(original))
	}
}

func TestGetIntParam(t *testing.T) {
	line := "=ybegin part=5 total=100 line=128 size=768000 name=file.mkv"
	if v := getIntParam(line, "part"); v != 5 {
		t.Errorf("part: got %d", v)
	}
	if v := getIntParam(line, "total"); v != 100 {
		t.Errorf("total: got %d", v)
	}
	if v := getIntParam(line, "size"); v != 768000 {
		t.Errorf("size: got %d", v)
	}
	if v := getIntParam(line, "missing"); v != 0 {
		t.Errorf("missing: got %d", v)
	}
}

func TestGetHexParam(t *testing.T) {
	line := "=yend size=1000 pcrc32=ABCD1234 crc32=deadbeef"
	if v := getHexParam(line, "pcrc32"); v != 0xABCD1234 {
		t.Errorf("pcrc32: got %08x, want ABCD1234", v)
	}
	if v := getHexParam(line, "crc32"); v != 0xdeadbeef {
		t.Errorf("crc32: got %08x, want deadbeef", v)
	}
}

// encodeForTest encodes data using yEnc for testing purposes.
func encodeForTest(data []byte) string {
	var buf bytes.Buffer
	for _, b := range data {
		encoded := byte((int(b) + 42) % 256)
		// Escape special bytes: NUL, LF, CR, '=', '.'
		switch encoded {
		case 0x00, 0x0A, 0x0D, 0x3D, 0x2E:
			buf.WriteByte('=')
			buf.WriteByte(byte((int(encoded) + 64) % 256))
		default:
			buf.WriteByte(encoded)
		}
	}
	return buf.String()
}

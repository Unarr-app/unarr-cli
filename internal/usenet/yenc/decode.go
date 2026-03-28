package yenc

import (
	"bufio"
	"bytes"
	"fmt"
	"hash/crc32"
	"io"
	"strconv"
	"strings"
)

// Part represents a decoded yEnc part (one NNTP article body).
type Part struct {
	Name   string // filename from =ybegin
	Number int    // part number (1-based)
	Total  int    // total parts (from =ybegin total=N)
	Begin  int64  // byte offset start (from =ypart begin=N, 1-based)
	End    int64  // byte offset end (from =ypart end=N, inclusive)
	Size   int64  // total file size (from =ybegin size=N)
	CRC32  uint32 // CRC32 of this part's data (from =yend pcrc32)
	Data   []byte // decoded binary data
}

// Decode reads a yEnc encoded article body and returns the decoded part.
// The reader should contain the raw article body (after NNTP BODY response).
func Decode(r io.Reader) (*Part, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024) // up to 10MB per article

	part := &Part{}

	// Phase 1: Find and parse =ybegin header
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "=ybegin ") {
			parseYBegin(part, line)
			break
		}
	}
	if part.Name == "" && part.Size == 0 {
		return nil, fmt.Errorf("yenc: no =ybegin header found")
	}

	// Phase 2: Find optional =ypart header (for multipart)
	// Peek at next line
	if scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "=ypart ") {
			parseYPart(part, line)
		} else {
			// Not a ypart line, decode it as data
			part.Data = append(part.Data, decodeLine(line)...)
		}
	}

	// Phase 3: Decode data lines until =yend
	hasher := crc32.NewIEEE()
	// Hash data we already decoded (if any from non-ypart line)
	if len(part.Data) > 0 {
		hasher.Write(part.Data)
	}

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "=yend") {
			parseYEnd(part, line)
			break
		}

		decoded := decodeLine(line)
		hasher.Write(decoded)
		part.Data = append(part.Data, decoded...)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("yenc: read error: %w", err)
	}

	// Verify CRC32 if provided
	if part.CRC32 != 0 {
		computed := hasher.Sum32()
		if computed != part.CRC32 {
			return nil, fmt.Errorf("yenc: CRC32 mismatch: expected %08x, got %08x", part.CRC32, computed)
		}
	}

	return part, nil
}

// DecodeBytes decodes a yEnc encoded byte slice.
func DecodeBytes(data []byte) (*Part, error) {
	return Decode(bytes.NewReader(data))
}

// decodeLine decodes a single line of yEnc data.
// yEnc encoding: each byte = (original + 42) % 256
// Escape character '=' followed by next byte: (escapedByte - 64 - 42) % 256
func decodeLine(line string) []byte {
	out := make([]byte, 0, len(line))
	escaped := false

	for i := 0; i < len(line); i++ {
		b := line[i]

		if escaped {
			// Escaped byte: subtract 106 (42 + 64)
			out = append(out, b-106)
			escaped = false
			continue
		}

		if b == '=' {
			escaped = true
			continue
		}

		// Normal byte: subtract 42
		out = append(out, b-42)
	}

	return out
}

// parseYBegin parses "=ybegin part=1 total=50 line=128 size=768000 name=file.mkv"
func parseYBegin(p *Part, line string) {
	p.Number = getIntParam(line, "part")
	p.Total = getIntParam(line, "total")
	p.Size = int64(getIntParam(line, "size"))

	// Name is special: it's everything after "name=" to end of line
	if idx := strings.Index(line, "name="); idx >= 0 {
		p.Name = strings.TrimSpace(line[idx+5:])
	}
}

// parseYPart parses "=ypart begin=1 end=768000"
func parseYPart(p *Part, line string) {
	p.Begin = int64(getIntParam(line, "begin"))
	p.End = int64(getIntParam(line, "end"))
}

// parseYEnd parses "=yend size=768000 part=1 pcrc32=ABCD1234 crc32=ABCD1234"
func parseYEnd(p *Part, line string) {
	// pcrc32 is the CRC of this part; crc32 is the CRC of the whole file (only on last part)
	if hex := getHexParam(line, "pcrc32"); hex != 0 {
		p.CRC32 = hex
	} else if hex := getHexParam(line, "crc32"); hex != 0 && p.Total <= 1 {
		// For single-part files, crc32 is the only CRC
		p.CRC32 = hex
	}
}

// getIntParam extracts an integer parameter from a yEnc header line.
func getIntParam(line, key string) int {
	prefix := key + "="
	idx := strings.Index(line, prefix)
	if idx < 0 {
		return 0
	}
	start := idx + len(prefix)
	end := start
	for end < len(line) && line[end] >= '0' && line[end] <= '9' {
		end++
	}
	if end == start {
		return 0
	}
	v, _ := strconv.Atoi(line[start:end])
	return v
}

// getHexParam extracts a hex parameter (like CRC32) from a yEnc header line.
// Uses word-boundary matching to avoid "pcrc32" matching "crc32".
func getHexParam(line, key string) uint32 {
	prefix := key + "="
	idx := strings.Index(line, prefix)
	if idx < 0 {
		return 0
	}
	// Ensure we're matching the exact key, not a suffix (e.g., "crc32" should not match "pcrc32")
	if idx > 0 && line[idx-1] != ' ' && line[idx-1] != '\t' {
		// Try finding another occurrence after this one
		rest := line[idx+1:]
		nextIdx := strings.Index(rest, prefix)
		if nextIdx < 0 {
			return 0
		}
		idx = idx + 1 + nextIdx
		if idx > 0 && line[idx-1] != ' ' && line[idx-1] != '\t' {
			return 0
		}
	}
	start := idx + len(prefix)
	end := start
	for end < len(line) && ((line[end] >= '0' && line[end] <= '9') ||
		(line[end] >= 'a' && line[end] <= 'f') ||
		(line[end] >= 'A' && line[end] <= 'F')) {
		end++
	}
	if end == start {
		return 0
	}
	v, err := strconv.ParseUint(line[start:end], 16, 32)
	if err != nil {
		return 0
	}
	return uint32(v)
}

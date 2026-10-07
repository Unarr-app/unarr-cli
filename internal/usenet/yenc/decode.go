package yenc

import (
	"bufio"
	"bytes"
	"fmt"
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
	var env envelope

	// Phase 1: Find and parse =ybegin header
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "=ybegin ") {
			env.begin(part, line)
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
			env.part(part, line)
		} else if strings.HasPrefix(line, "=yend ") {
			env.trailer(part, line)
		} else {
			// Not a ypart line, decode it as data
			part.Data = append(part.Data, decodeLine(line)...)
		}
	}

	// Phase 3: Decode data lines until =yend
	for !env.ended && scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "=yend ") {
			env.trailer(part, line)
			break
		}

		decoded := decodeLine(line)
		part.Data = append(part.Data, decoded...)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("yenc: read error: %w", err)
	}

	if err := env.validate(part); err != nil {
		return nil, err
	}

	normalizeSinglePart(part)
	return part, nil
}

// normalizeSinglePart fills in the byte range a single-part article implies.
// Single-part yEnc bodies carry no =ypart header, so Begin/End stay zero even
// though the article IS the whole file. Synthesizing Begin=1, End=len(Data)
// (and Size when =ybegin omitted it) makes every decoded Part self-consistent —
// the streaming OffsetIndex/Reader can then treat single- and multi-part
// articles uniformly instead of reading a lone-article file as empty. Multipart
// parts already have a non-zero range from =ypart and are left untouched.
func normalizeSinglePart(part *Part) {
	if part.Begin != 0 || part.End != 0 || len(part.Data) == 0 {
		return
	}
	part.Begin = 1
	part.End = int64(len(part.Data))
	if part.Size == 0 {
		part.Size = part.End
	}
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

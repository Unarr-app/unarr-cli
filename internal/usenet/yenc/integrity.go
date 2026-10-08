package yenc

import (
	"fmt"
	"hash/crc32"
	"strconv"
	"strings"
)

// envelope is private decoding state. In-flight Progress may expose a prefix;
// only a complete, consistent envelope can become a successful cached Part.
type envelope struct {
	ended, checksumPresent, hasRange bool
	length                           int64
	number                           int
	err                              error
}

func (e *envelope) begin(p *Part, line string) {
	parseYBegin(p, line)
	for _, key := range []string{"size", "part", "total"} {
		if _, present := paramValue(line, key); !present {
			continue
		}
		if _, err := requiredDecimal(line, key); err != nil {
			e.err = err
			return
		}
	}
}

func (e *envelope) part(p *Part, line string) {
	e.hasRange = true
	var err error
	p.Begin, err = requiredDecimal(line, "begin")
	if err != nil {
		e.err = err
		return
	}
	p.End, err = requiredDecimal(line, "end")
	if err != nil {
		e.err = err
	}
}

func (e *envelope) trailer(p *Part, line string) {
	e.ended = true
	var err error
	e.length, err = requiredDecimal(line, "size")
	if err != nil {
		e.err = err
		return
	}
	if p.Number != 0 {
		n, err := requiredDecimal(line, "part")
		if err != nil {
			e.err = err
			return
		}
		e.number = int(n)
	}
	for _, key := range []string{"pcrc32", "crc32"} {
		value, present := paramValue(line, key)
		if !present {
			continue
		}
		crc, err := strconv.ParseUint(value, 16, 32)
		if err != nil || len(value) != 8 {
			e.err = fmt.Errorf("yenc: invalid %s", key)
			return
		}
		if !e.checksumPresent && (key == "pcrc32" || p.Total <= 1) {
			p.CRC32, e.checksumPresent = uint32(crc), true
		}
	}
}

func (e *envelope) validate(p *Part) error {
	if e.err != nil {
		return e.err
	}
	if !e.ended {
		return fmt.Errorf("yenc: missing =yend trailer")
	}
	if e.length != int64(len(p.Data)) {
		return fmt.Errorf("yenc: trailer size differs from decoded length")
	}
	if err := e.validateBounds(p); err != nil {
		return err
	}
	if e.checksumPresent {
		if computed := crc32.ChecksumIEEE(p.Data); computed != p.CRC32 {
			return fmt.Errorf("yenc: CRC32 mismatch: expected %08x, got %08x", p.CRC32, computed)
		}
	}
	return nil
}

func (e *envelope) validateBounds(p *Part) error {
	if p.Size < 0 {
		return fmt.Errorf("yenc: invalid file size")
	}
	if p.Number != 0 || p.Total > 1 || e.hasRange {
		return e.validateMultipart(p)
	}
	if p.Size != 0 && p.Size != e.length {
		return fmt.Errorf("yenc: file size differs from decoded length")
	}
	return nil
}

func (e *envelope) validateMultipart(p *Part) error {
	if !e.hasRange || p.Begin < 1 || p.End < p.Begin || p.End > p.Size || p.End-p.Begin+1 != e.length {
		return fmt.Errorf("yenc: invalid multipart bounds")
	}
	if p.Number < 1 || (p.Total > 0 && p.Number > p.Total) || e.number != p.Number {
		return fmt.Errorf("yenc: inconsistent multipart number")
	}
	return nil
}

func paramValue(line, key string) (string, bool) {
	for line != "" {
		line = strings.TrimLeft(line, " \t")
		end := strings.IndexAny(line, " \t")
		if end < 0 {
			end = len(line)
		}
		field := line[:end]
		line = line[end:]
		if strings.HasPrefix(field, "name=") {
			return "", false
		}
		if value, ok := strings.CutPrefix(field, key+"="); ok {
			return value, true
		}
	}
	return "", false
}

func requiredDecimal(line, key string) (int64, error) {
	value, ok := paramValue(line, key)
	if !ok || value == "" {
		return 0, fmt.Errorf("yenc: missing %s", key)
	}
	for _, b := range []byte(value) {
		if b < '0' || b > '9' {
			return 0, fmt.Errorf("yenc: invalid %s", key)
		}
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("yenc: invalid %s", key)
	}
	return n, nil
}

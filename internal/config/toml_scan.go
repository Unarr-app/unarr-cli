package config

import (
	"strconv"
	"strings"
)

// A deliberately small TOML line scanner for Save's in-place merge: it locates
// `key = value` spans and table headers so values can be swapped without
// touching comments or layout. It is NOT a validator — anything it does not
// understand makes it bail out, and every merged result is re-decoded and
// compared against the intended values before it is written (see mergeConfigText).

// keySep joins a key path into a map key; it cannot appear in a TOML key we write.
const keySep = "\x1f"

// bom is the UTF-8 byte-order mark older Notepad versions prefix files with.
const bom = "\xef\xbb\xbf"

func pathKey(p []string) string { return strings.Join(p, keySep) }

// tomlEntry is the span of one key/value pair in tomlDoc.lines.
type tomlEntry struct {
	first, last int // line span of the key/value
	valStart    int // byte offset of the value in lines[first]
	valEnd      int // byte offset just past the value in lines[last]
}

type tomlDoc struct {
	lines   []string
	entries map[string]tomlEntry
	order   []string            // entry keys in file order
	paths   map[string][]string // entry key → its key path
	tables  map[string]int      // table key → last line of its section ("" = root; -1 = before line 0)
	headers map[string]int      // table key → its [header] line
	dotted  map[string]bool     // tables defined by dotted keys; TOML forbids a [header] for them
}

func scanDoc(text string) (*tomlDoc, bool) {
	d := &tomlDoc{
		lines:   strings.Split(text, "\n"),
		entries: map[string]tomlEntry{},
		paths:   map[string][]string{},
		tables:  map[string]int{"": -1},
		headers: map[string]int{},
		dotted:  map[string]bool{},
	}
	var table []string
	arrayTable := false
	for i := 0; i < len(d.lines); i++ {
		line := d.lines[i]
		rest := strings.TrimLeft(line, " \t\r"+bom)
		switch {
		case rest == "" || rest[0] == '#':
		case strings.HasPrefix(rest, "[["):
			arrayTable = true // not in the schema: its entries are skipped, never edited
		case rest[0] == '[':
			p, after, ok := parseKey(rest[1:])
			if !ok || !closesHeader(after) {
				return nil, false
			}
			table, arrayTable = p, false
			d.headers[pathKey(p)] = i
			d.tables[pathKey(p)] = i
		default:
			last, ok := d.addEntry(i, len(line)-len(rest), table, arrayTable)
			if !ok {
				return nil, false
			}
			i = last
		}
	}
	return d, true
}

func closesHeader(after string) bool {
	if !strings.HasPrefix(after, "]") {
		return false
	}
	tail := strings.TrimSpace(after[1:])
	return tail == "" || tail[0] == '#'
}

// addEntry scans the key/value starting at lines[i][off:] and returns its last line.
func (d *tomlDoc) addEntry(i, off int, table []string, skip bool) (int, bool) {
	line := d.lines[i]
	keyParts, after, ok := parseKey(line[off:])
	if !ok || !strings.HasPrefix(after, "=") {
		return 0, false
	}
	start := len(line) - len(strings.TrimLeft(after[1:], " \t"))
	last, end, ok := scanValue(d.lines, i, start)
	if !ok || skip {
		return last, ok
	}
	p := append(append([]string{}, table...), keyParts...)
	k := pathKey(p)
	d.entries[k] = tomlEntry{first: i, last: last, valStart: start, valEnd: end}
	d.paths[k] = p
	d.order = append(d.order, k)
	d.tables[pathKey(table)] = last
	for j := len(table) + 1; j < len(p); j++ {
		d.dotted[pathKey(p[:j])] = true
	}
	return last, true
}

// valueText returns the raw source text of e's value.
func (d *tomlDoc) valueText(e tomlEntry) string {
	if e.first == e.last {
		return d.lines[e.first][e.valStart:e.valEnd]
	}
	parts := []string{d.lines[e.first][e.valStart:]}
	parts = append(parts, d.lines[e.first+1:e.last]...)
	return strings.Join(append(parts, d.lines[e.last][:e.valEnd]), "\n")
}

// parseKey parses a (dotted) key and returns its parts and the remaining text,
// left-trimmed. The remainder is always a suffix of s so callers can compute offsets.
func parseKey(s string) ([]string, string, bool) {
	var parts []string
	for {
		part, rest, ok := parseKeyPart(strings.TrimLeft(s, " \t"))
		if !ok {
			return nil, "", false
		}
		parts = append(parts, part)
		rest = strings.TrimLeft(rest, " \t")
		if !strings.HasPrefix(rest, ".") {
			return parts, rest, true
		}
		s = rest[1:]
	}
}

func parseKeyPart(s string) (string, string, bool) {
	if s == "" {
		return "", "", false
	}
	switch s[0] {
	case '"':
		end := closingQuote(s)
		if end < 0 {
			return "", "", false
		}
		v, err := strconv.Unquote(s[:end+1])
		return v, s[end+1:], err == nil
	case '\'':
		end := strings.IndexByte(s[1:], '\'')
		if end < 0 {
			return "", "", false
		}
		return s[1 : end+1], s[end+2:], true
	}
	n := 0
	for n < len(s) && isBareKeyChar(s[n]) {
		n++
	}
	return s[:n], s[n:], n > 0
}

// closingQuote returns the index of the quote closing the basic string at s[0].
func closingQuote(s string) int {
	for j := 1; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case '"':
			return j
		}
	}
	return -1
}

func isBareKeyChar(c byte) bool {
	return c == '_' || c == '-' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

const (
	stNormal = iota
	stBasic
	stLiteral
	stMLBasic
	stMLLiteral
)

// valueLexer tracks string/bracket state across the lines of one value.
type valueLexer struct {
	state int
	depth int
}

// scanValue finds where the value starting at lines[i][col] ends: its last line
// and the byte offset just past it (trailing whitespace and comment excluded).
func scanValue(lines []string, i, col int) (int, int, bool) {
	var lx valueLexer
	for ; i < len(lines); i, col = i+1, 0 {
		end, ok := lx.line(lines[i], col)
		if !ok {
			return 0, 0, false
		}
		if lx.state == stNormal && lx.depth == 0 {
			return i, end, true
		}
	}
	return 0, 0, false
}

func (lx *valueLexer) line(s string, col int) (int, bool) {
	end := col
	for j := col; j < len(s); j++ {
		c := s[j]
		if lx.state == stNormal && c == '#' {
			break
		}
		j += lx.step(s, j)
		if c != ' ' && c != '\t' && c != '\r' {
			end = min(j+1, len(s))
		}
	}
	// A single-line string may not run past the end of its line.
	return end, lx.state != stBasic && lx.state != stLiteral
}

// step consumes s[j] and returns how many extra bytes it swallowed.
func (lx *valueLexer) step(s string, j int) int {
	c := s[j]
	switch lx.state {
	case stNormal:
		return lx.normal(s, j)
	case stBasic, stMLBasic:
		if c == '\\' {
			return 1
		}
	}
	q, n := byte('"'), 1
	switch lx.state {
	case stLiteral:
		q = '\''
	case stMLBasic:
		n = 3
	case stMLLiteral:
		q, n = '\'', 3
	}
	delim := strings.Repeat(string(q), n)
	if !strings.HasPrefix(s[j:], delim) {
		return 0
	}
	lx.state = stNormal
	extra := n - 1
	// A multi-line string may end with up to two quotes of content before its delimiter.
	for n == 3 && extra < 4 && j+extra+1 < len(s) && s[j+extra+1] == q {
		extra++
	}
	return extra
}

func (lx *valueLexer) normal(s string, j int) int {
	switch c := s[j]; c {
	case '[', '{':
		lx.depth++
	case ']', '}':
		lx.depth--
	case '"', '\'':
		ml := strings.HasPrefix(s[j:], strings.Repeat(string(c), 3))
		switch {
		case c == '"' && ml:
			lx.state = stMLBasic
		case c == '"':
			lx.state = stBasic
		case ml:
			lx.state = stMLLiteral
		default:
			lx.state = stLiteral
		}
		if ml {
			return 2
		}
	}
	return 0
}

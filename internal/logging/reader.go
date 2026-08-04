package logging

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"
)

// DefaultLines is how much history `unarr logs` shows when asked for none.
const DefaultLines = 50

// maxLineBytes caps a single log line. ffmpeg and the torrent client can emit
// very long records; the stdlib default (64 KiB) would abort the scan on one,
// which would look like "the log stops here".
const maxLineBytes = 1 << 20

// tailRingInitialCap bounds the tail ring's FIRST allocation. Anything larger
// is grown into only as matching lines actually arrive, so an absurd -n costs
// nothing until the log really holds that much.
const tailRingInitialCap = 4096

// Query selects which log lines to show. The zero value is meaningless — Path
// is required; everything else has a documented default.
type Query struct {
	Path     string    // live log file
	Lines    int       // last N matching lines; <=0 = DefaultLines
	MinLevel Level     // drop anything less severe
	Grep     string    // case-insensitive regular expression; empty = no filter
	Since    time.Time // drop anything older; zero = no time filter
	Format   Format    // how Print renders what it kept
	MaxFiles int       // rotated siblings to walk back through; <=0 = DefaultMaxFiles
}

// Validate reports whether the query can be run at all, without opening a file
// or spawning anything. The CLI calls it while it is still parsing flags, so a
// bad --grep fails there instead of after a reader — or a journalctl child
// process — is already live: the filter compiles the pattern lazily, and a
// caller that bailed out before reading a byte would leave that process holding
// the pipe.
func (q Query) Validate() error {
	_, err := q.compile()
	return err
}

// lines is the resolved history depth.
func (q Query) lines() int {
	if q.Lines <= 0 {
		return DefaultLines
	}
	return q.Lines
}

// sources lists the files to read, newest first: the live log, then its
// rotated siblings. Rotation means "the last 200 lines" can legitimately span
// two files, and a reader that only knew about unarr.log would silently show
// less than it was asked for right after a rotation.
func (q Query) sources() []string {
	return append([]string{q.Path}, RotatedPaths(q.Path, q.MaxFiles)...)
}

// matcher is a Query with its pattern compiled once, instead of per line.
type matcher struct {
	minLevel Level
	since    time.Time
	re       *regexp.Regexp
}

// compile prepares the per-line filter, reporting a bad --grep pattern up front
// rather than silently matching nothing.
func (q Query) compile() (matcher, error) {
	m := matcher{minLevel: q.MinLevel, since: q.Since}
	if q.Grep == "" {
		return m, nil
	}
	re, err := regexp.Compile("(?i)" + q.Grep)
	if err != nil {
		return m, fmt.Errorf("invalid --grep pattern %q: %w", q.Grep, err)
	}
	m.re = re
	return m, nil
}

// lineFilter turns raw lines into entries and decides which survive. It carries
// the last timestamp it saw forward, so continuation lines (stack traces,
// ffmpeg spew) are judged by the time of the record they belong to instead of
// being dropped by --since for having no stamp of their own.
type lineFilter struct {
	m    matcher
	last time.Time
}

// accept parses one raw line and reports whether the query keeps it.
func (lf *lineFilter) accept(line string) (Entry, bool) {
	e := ParseEntry(line)
	if e.Time.IsZero() {
		e.Time = lf.last
	} else {
		lf.last = e.Time
	}
	if !e.Level.Enabled(lf.m.minLevel) {
		return e, false
	}
	if !lf.m.since.IsZero() && !e.Time.IsZero() && e.Time.Before(lf.m.since) {
		return e, false
	}
	if lf.m.re != nil && !lf.m.re.MatchString(e.Raw) {
		return e, false
	}
	return e, true
}

// Read returns the last q.Lines matching entries, oldest first, walking back
// through rotated files only when the live one does not hold enough.
func Read(q Query) ([]Entry, error) {
	m, err := q.compile()
	if err != nil {
		return nil, err
	}
	want := q.lines()
	var out []Entry
	for _, path := range q.sources() {
		chunk, err := tailMatches(path, want-len(out), m)
		if err != nil {
			return nil, err
		}
		out = append(chunk, out...) // older file's lines belong in front
		if len(out) >= want {
			break
		}
	}
	return out, nil
}

// Print writes the entries Read selected, one rendered line each.
func Print(q Query, w io.Writer) error {
	entries, err := Read(q)
	if err != nil {
		return err
	}
	return printEntries(entries, q.Format, w)
}

// FilterTail applies a query to output that does NOT come from the log file —
// journalctl's, on a systemd box — and writes the last q.Lines matches. Same
// filter as the file reader, so --level / --grep / --since mean exactly the
// same thing whichever source the platform happens to use.
func FilterTail(r io.Reader, q Query, w io.Writer) error {
	m, err := q.compile()
	if err != nil {
		return err
	}
	entries, err := tailFrom(newScanner(r), q.lines(), m)
	if err != nil {
		return err
	}
	return printEntries(entries, q.Format, w)
}

// FilterLive is FilterTail for a stream that never ends (journalctl -f): each
// matching line is written as it arrives instead of being held for a tail.
func FilterLive(r io.Reader, q Query, w io.Writer) error {
	m, err := q.compile()
	if err != nil {
		return err
	}
	lf := lineFilter{m: m}
	sc := newScanner(r)
	for sc.Scan() {
		e, ok := lf.accept(sc.Text())
		if !ok {
			continue
		}
		if _, err := fmt.Fprintln(w, q.Format.Render(e)); err != nil {
			return err
		}
	}
	return sc.Err()
}

// printEntries renders a batch of entries, one line each.
func printEntries(entries []Entry, f Format, w io.Writer) error {
	for _, e := range entries {
		if _, err := fmt.Fprintln(w, f.Render(e)); err != nil {
			return err
		}
	}
	return nil
}

// tailMatches returns the last n matching entries of one file, oldest first.
// A missing file is not an error: the rotated slots simply do not exist yet on
// a fresh install.
func tailMatches(path string, n int, m matcher) ([]Entry, error) {
	if n <= 0 {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open log file: %w", err)
	}
	defer f.Close()
	return tailFrom(newScanner(f), n, m)
}

// tailFrom keeps the last n matching entries a scanner yields, oldest first.
func tailFrom(sc *bufio.Scanner, n int, m matcher) ([]Entry, error) {
	// Bounded ring: memory is O(n), not O(input), so tailing a 20 MB log costs
	// the same as tailing an empty one. The initial capacity is NOT n, though —
	// n comes straight off `-n`, and pre-sizing by it turns `unarr logs -n
	// 1099511627776` into an unrecoverable out-of-memory abort. The ring still
	// reaches n when the input really holds that many matches; it just grows
	// into it as lines arrive.
	ring := make([]Entry, 0, min(n, tailRingInitialCap))
	lf := lineFilter{m: m}
	for sc.Scan() {
		e, ok := lf.accept(sc.Text())
		if !ok {
			continue
		}
		if len(ring) == n {
			// Shift in place rather than resliceing: ring[1:] would shrink the
			// capacity and make every further append reallocate.
			copy(ring, ring[1:])
			ring = ring[:n-1]
		}
		ring = append(ring, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read log file: %w", err)
	}
	return ring, nil
}

// newScanner builds a line scanner sized for real daemon output.
func newScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	return sc
}

package logging

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// writeLines seeds a log file with one line per element.
func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
}

// printed runs a query and returns the rendered output as lines.
func printed(t *testing.T, q Query) []string {
	t.Helper()
	var buf bytes.Buffer
	if err := Print(q, &buf); err != nil {
		t.Fatalf("Print: %v", err)
	}
	out := strings.TrimSuffix(buf.String(), "\n")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func TestReadReturnsTheLastLinesOldestFirst(t *testing.T) {
	path := newTestLog(t)
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, "line "+strconv.Itoa(i))
	}
	writeLines(t, path, lines...)

	got := printed(t, Query{Path: path, Lines: 3})
	want := []string{"line 8", "line 9", "line 10"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestReadSurvivesAnAbsurdLineCount(t *testing.T) {
	// Regression: the tail ring was pre-sized to -n, so `unarr logs -n
	// 1099511627776` aborted the process with "runtime: out of memory" — a stack
	// dump instead of an answer. The ring must cost what the log holds, not what
	// the flag asked for.
	path := newTestLog(t)
	writeLines(t, path, "line 1", "line 2", "line 3")

	got := printed(t, Query{Path: path, Lines: 1 << 40})
	want := []string{"line 1", "line 2", "line 3"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want every line the log actually holds (%v)", got, want)
	}
}

func TestValidateReportsABadGrepWithoutTouchingTheLog(t *testing.T) {
	// The CLI calls this while parsing flags, before it spawns journalctl.
	if err := (Query{Path: "/nonexistent/unarr.log", Grep: "["}).Validate(); err == nil {
		t.Fatal("an uncompilable pattern must be reported by Validate")
	}
	if err := (Query{Path: "/nonexistent/unarr.log", Grep: "nzb|usenet"}).Validate(); err != nil {
		t.Fatalf("a valid query must not need the file to exist: %v", err)
	}
}

func TestReadContinuesIntoRotatedFiles(t *testing.T) {
	path := newTestLog(t)
	// Oldest history lives in .2, then .1, then the live file — exactly the
	// layout rotation leaves behind.
	writeLines(t, RotatedPath(path, 2), "old 1", "old 2")
	writeLines(t, RotatedPath(path, 1), "mid 1", "mid 2")
	writeLines(t, path, "new 1")

	got := printed(t, Query{Path: path, Lines: 4, MaxFiles: 3})
	want := []string{"old 2", "mid 1", "mid 2", "new 1"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestReadStopsAtTheLiveFileWhenItHasEnough(t *testing.T) {
	path := newTestLog(t)
	writeLines(t, RotatedPath(path, 1), "old")
	writeLines(t, path, "a", "b", "c")

	got := printed(t, Query{Path: path, Lines: 2, MaxFiles: 3})
	if strings.Join(got, "|") != "b|c" {
		t.Fatalf("got %v, want the live tail only", got)
	}
}

func TestReadOnAMissingLogIsEmptyNotAnError(t *testing.T) {
	if got := printed(t, Query{Path: newTestLog(t), Lines: 5}); got != nil {
		t.Fatalf("got %v, want nothing", got)
	}
}

func TestLevelFilterDropsLessSevereLines(t *testing.T) {
	path := newTestLog(t)
	writeLines(t, path,
		"2025/01/20 10:00:00 [sync] tick",
		"2025/01/20 10:00:01 [usenet] WARNING: par2 missing",
		"2025/01/20 10:00:02 Error: could not reach the API",
		"2025/01/20 10:00:03 [sync] tick",
	)

	got := printed(t, Query{Path: path, Lines: 50, MinLevel: LevelWarn})
	if len(got) != 2 {
		t.Fatalf("got %d lines, want the warn + the error: %v", len(got), got)
	}

	got = printed(t, Query{Path: path, Lines: 50, MinLevel: LevelError})
	if len(got) != 1 || !strings.Contains(got[0], "could not reach") {
		t.Fatalf("got %v, want only the error line", got)
	}
}

func TestGrepKeepsOnlyMatchingLinesCaseInsensitively(t *testing.T) {
	path := newTestLog(t)
	writeLines(t, path, "downloading NZB 12", "seeding torrent", "nzb finished")

	got := printed(t, Query{Path: path, Lines: 50, Grep: "nzb"})
	if len(got) != 2 {
		t.Fatalf("got %v, want both nzb lines regardless of case", got)
	}

	got = printed(t, Query{Path: path, Lines: 50, Grep: "^seeding|finished$"})
	if len(got) != 2 {
		t.Fatalf("got %v, want the regular expression honoured", got)
	}
}

func TestAnInvalidGrepPatternIsReported(t *testing.T) {
	path := newTestLog(t)
	writeLines(t, path, "anything")
	if err := Print(Query{Path: path, Grep: "("}, &bytes.Buffer{}); err == nil {
		t.Fatal("a broken regular expression must be reported, not matched against nothing")
	}
}

func TestSinceDropsOlderLines(t *testing.T) {
	path := newTestLog(t)
	writeLines(t, path,
		"2025/01/20 09:00:00 early",
		"2025/01/20 11:00:00 late",
	)
	cut := time.Date(2025, time.January, 20, 10, 0, 0, 0, time.Local)

	got := printed(t, Query{Path: path, Lines: 50, Since: cut})
	if len(got) != 1 || !strings.Contains(got[0], "late") {
		t.Fatalf("got %v, want only the line after the cut-off", got)
	}
}

func TestSinceKeepsContinuationLinesWithTheirRecord(t *testing.T) {
	path := newTestLog(t)
	// A stack trace: only the first line carries a stamp. Dropping the rest
	// would mangle exactly the output someone uses --since to find.
	writeLines(t, path,
		"2025/01/20 09:00:00 early",
		"2025/01/20 11:00:00 panic: boom",
		"    goroutine 1 [running]:",
		"    main.main()",
	)
	cut := time.Date(2025, time.January, 20, 10, 0, 0, 0, time.Local)

	got := printed(t, Query{Path: path, Lines: 50, Since: cut})
	if len(got) != 3 {
		t.Fatalf("got %v, want the panic and its two continuation lines", got)
	}
}

func TestJSONFormatRendersEveryKeptLine(t *testing.T) {
	path := newTestLog(t)
	writeLines(t, path, "2025/01/20 10:00:00 hello")

	got := printed(t, Query{Path: path, Lines: 5, Format: FormatJSON})
	if len(got) != 1 || !strings.HasPrefix(got[0], `{"time":`) {
		t.Fatalf("got %v, want one json-lines record", got)
	}
}

func TestReadHandlesALineLongerThanTheScannerDefault(t *testing.T) {
	path := newTestLog(t)
	long := strings.Repeat("x", 128*1024)
	writeLines(t, path, "short", long)

	got := printed(t, Query{Path: path, Lines: 5})
	if len(got) != 2 || len(got[1]) != len(long) {
		t.Fatalf("a 128 KiB line must not abort the read (got %d lines)", len(got))
	}
}

package logging

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// journalOutput is what `journalctl -o cat` hands us for the unarr unit: the
// daemon's own lines, stamped by log.Printf.
const journalOutput = `2025/01/20 10:00:00 [sync] tick
2025/01/20 10:00:01 [usenet] WARNING: par2 not found in PATH
2025/01/20 10:00:02 Error: could not reach the API
2025/01/20 10:00:03 [sync] tick
`

func TestFilterTailAppliesTheSameLevelFilterAsTheFileReader(t *testing.T) {
	var out bytes.Buffer
	q := Query{Lines: 50, MinLevel: LevelWarn}
	if err := FilterTail(strings.NewReader(journalOutput), q, &out); err != nil {
		t.Fatalf("FilterTail: %v", err)
	}
	got := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(got) != 2 {
		t.Fatalf("got %v, want the warning and the error only", got)
	}
}

func TestFilterTailKeepsOnlyTheLastMatches(t *testing.T) {
	var out bytes.Buffer
	if err := FilterTail(strings.NewReader(journalOutput), Query{Lines: 1}, &out); err != nil {
		t.Fatalf("FilterTail: %v", err)
	}
	if got := strings.TrimSpace(out.String()); !strings.HasSuffix(got, "[sync] tick") {
		t.Fatalf("got %q, want the last line", got)
	}
}

func TestFilterTailHonoursGrepAndSince(t *testing.T) {
	var out bytes.Buffer
	q := Query{
		Lines: 50,
		Grep:  "par2",
		Since: time.Date(2025, time.January, 20, 9, 0, 0, 0, time.Local),
	}
	if err := FilterTail(strings.NewReader(journalOutput), q, &out); err != nil {
		t.Fatalf("FilterTail: %v", err)
	}
	if got := strings.TrimSpace(out.String()); !strings.Contains(got, "par2") || strings.Contains(got, "[sync]") {
		t.Fatalf("got %q, want only the par2 line", got)
	}
}

func TestFilterTailReportsABadPattern(t *testing.T) {
	if err := FilterTail(strings.NewReader(journalOutput), Query{Grep: "("}, &bytes.Buffer{}); err == nil {
		t.Fatal("a broken --grep must be reported here too, not only on the file path")
	}
}

func TestFilterLiveWritesEachMatchAsItArrives(t *testing.T) {
	var out bytes.Buffer
	q := Query{MinLevel: LevelError, Format: FormatJSON}
	if err := FilterLive(strings.NewReader(journalOutput), q, &out); err != nil {
		t.Fatalf("FilterLive: %v", err)
	}
	got := strings.TrimSpace(out.String())
	if strings.Count(got, "\n") != 0 {
		t.Fatalf("got %q, want exactly one record", got)
	}
	if !strings.Contains(got, `"level":"error"`) {
		t.Fatalf("got %q, want the error rendered as json-lines", got)
	}
}

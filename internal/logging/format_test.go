package logging

import (
	"encoding/json"
	"testing"
	"time"
)

func TestParseFormatAcceptsTheUsualSpellings(t *testing.T) {
	cases := map[string]Format{
		"":      DefaultFormat,
		"text":  FormatText,
		"PLAIN": FormatText,
		"json":  FormatJSON,
		"jsonl": FormatJSON,
	}
	for in, want := range cases {
		got, err := ParseFormat(in)
		if err != nil {
			t.Fatalf("ParseFormat(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("ParseFormat(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := ParseFormat("yaml"); err == nil {
		t.Fatal("an unsupported format must fail loudly")
	}
}

func TestParseEntrySplitsTheStdlibLogStamp(t *testing.T) {
	e := ParseEntry("2025/01/20 10:11:12 [usenet] WARNING: par2 missing")
	if e.Time.IsZero() {
		t.Fatal("the log.Printf stamp was not recognised")
	}
	if y, m, d := e.Time.Date(); y != 2025 || m != time.January || d != 20 {
		t.Fatalf("parsed date %v, want 2025-01-20", e.Time)
	}
	if e.Level != LevelWarn {
		t.Fatalf("level = %v, want warn", e.Level)
	}
	if e.Message != "[usenet] WARNING: par2 missing" {
		t.Fatalf("message = %q, want the stamp stripped", e.Message)
	}
}

func TestParseEntryKeepsAnUnparseableLineWhole(t *testing.T) {
	const raw = "  ffmpeg spew with no stamp at all"
	e := ParseEntry(raw)
	if !e.Time.IsZero() {
		t.Fatalf("invented a timestamp: %v", e.Time)
	}
	if e.Message != raw || e.Raw != raw {
		t.Fatalf("message = %q, want the whole line preserved", e.Message)
	}
}

func TestTextRenderIsByteIdenticalToTheSource(t *testing.T) {
	const raw = "2025/01/20 10:11:12  spaced   oddly  "
	if got := FormatText.Render(ParseEntry(raw)); got != raw {
		t.Fatalf("text render = %q, want the source line verbatim", got)
	}
}

func TestJSONRenderEmitsOneRecordPerLine(t *testing.T) {
	got := FormatJSON.Render(ParseEntry("2025/01/20 10:11:12 Error: boom"))

	var rec struct {
		Time    string `json:"time"`
		Level   string `json:"level"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(got), &rec); err != nil {
		t.Fatalf("not valid JSON (%v): %s", err, got)
	}
	if rec.Level != "error" {
		t.Fatalf("level = %q, want error", rec.Level)
	}
	if rec.Message != "Error: boom" {
		t.Fatalf("message = %q", rec.Message)
	}
	if rec.Time == "" {
		t.Fatal("a stamped line must carry its time into JSON")
	}
}

func TestJSONRenderOmitsAnAbsentTimestamp(t *testing.T) {
	got := FormatJSON.Render(ParseEntry("no stamp here"))
	var rec map[string]any
	if err := json.Unmarshal([]byte(got), &rec); err != nil {
		t.Fatalf("not valid JSON (%v): %s", err, got)
	}
	if _, ok := rec["time"]; ok {
		t.Fatal("a line with no timestamp must not get a zero date")
	}
}

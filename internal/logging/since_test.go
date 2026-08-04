package logging

import (
	"testing"
	"time"
)

func TestParseSinceReadsRelativeAges(t *testing.T) {
	now := time.Date(2025, time.January, 20, 12, 0, 0, 0, time.Local)
	cases := map[string]time.Duration{
		"30m": 30 * time.Minute,
		"2h":  2 * time.Hour,
		"90s": 90 * time.Second,
		"7d":  7 * 24 * time.Hour,
		"1w":  7 * 24 * time.Hour,
	}
	for in, back := range cases {
		got, err := ParseSince(in, now)
		if err != nil {
			t.Fatalf("ParseSince(%q): %v", in, err)
		}
		if want := now.Add(-back); !got.Equal(want) {
			t.Fatalf("ParseSince(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseSinceReadsAbsoluteStamps(t *testing.T) {
	now := time.Date(2025, time.January, 20, 12, 0, 0, 0, time.Local)

	got, err := ParseSince("2025-01-19 08:30:00", now)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	want := time.Date(2025, time.January, 19, 8, 30, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	// A date alone means midnight that day.
	got, err = ParseSince("2025-01-19", now)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if !got.Equal(time.Date(2025, time.January, 19, 0, 0, 0, 0, time.Local)) {
		t.Fatalf("bare date parsed as %v", got)
	}
}

func TestParseSinceTreatsATimeAloneAsToday(t *testing.T) {
	now := time.Date(2025, time.January, 20, 12, 0, 0, 0, time.Local)
	got, err := ParseSince("09:15", now)
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	want := time.Date(2025, time.January, 20, 9, 15, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v — a bare time means today", got, want)
	}
}

func TestParseSinceEmptyMeansNoFilter(t *testing.T) {
	got, err := ParseSince("  ", time.Now())
	if err != nil {
		t.Fatalf("an empty --since is not an error: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("got %v, want the zero time that means 'no time filter'", got)
	}
}

func TestParseSinceRejectsNonsense(t *testing.T) {
	for _, in := range []string{"yesterday", "5 bananas", "-2h", "2025-13-45"} {
		if _, err := ParseSince(in, time.Now()); err == nil {
			t.Fatalf("ParseSince(%q) should have failed", in)
		}
	}
}

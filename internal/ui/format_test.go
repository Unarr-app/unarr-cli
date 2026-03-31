package ui

import (
	"fmt"
	"testing"
	"time"
)

func TestFormatSize(t *testing.T) {
	tests := []struct {
		name  string
		input *int64
		want  string
	}{
		{"nil", nil, "?"},
		{"zero", ptr(int64(0)), "0 B"},
		{"bytes", ptr(int64(500)), "500 B"},
		{"kilobytes", ptr(int64(1024)), "1.0 KB"},
		{"megabytes", ptr(int64(52428800)), "50.0 MB"},
		{"gigabytes", ptr(int64(4294967296)), "4.0 GB"},
		{"terabyte", ptr(int64(1099511627776)), "1.0 TB"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatSize(tt.input)
			if got != tt.want {
				t.Errorf("FormatSize(%v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		input int64
		want  string
	}{
		{0, "0 B"},
		{100, "100 B"},
		{1024, "1.0 KB"},
		{1048576, "1.0 MB"},
		{1073741824, "1.0 GB"},
		{1099511627776, "1.0 TB"},
		{3221225472, "3.0 GB"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := FormatBytes(tt.input)
			if got != tt.want {
				t.Errorf("FormatBytes(%d) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestFormatYear(t *testing.T) {
	tests := []struct {
		input *int
		want  string
	}{
		{nil, "-"},
		{intPtr(2023), "2023"},
		{intPtr(1999), "1999"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := FormatYear(tt.input)
			if got != tt.want {
				t.Errorf("FormatYear(%v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestFormatNumber(t *testing.T) {
	tests := []struct {
		input int
		want  string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1,000"},
		{1234567, "1,234,567"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := FormatNumber(tt.input)
			if got != tt.want {
				t.Errorf("FormatNumber(%d) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestTruncateString(t *testing.T) {
	tests := []struct {
		input  string
		maxLen int
		want   string
	}{
		{"short", 10, "short"},
		{"exactly10!", 10, "exactly10!"},
		{"this is too long", 10, "this is..."},
		{"ab", 5, "ab"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := TruncateString(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("TruncateString(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
}

func TestQualityIndicator(t *testing.T) {
	tests := []struct {
		name  string
		input *int
		want  string
	}{
		{"nil", nil, "  "},
		{"low", intPtr(30), "🔴"},
		{"medium", intPtr(60), "🟡"},
		{"high", intPtr(80), "🟢"},
		{"perfect", intPtr(100), "🟢"},
		{"boundary_40", intPtr(40), "🟡"},
		{"boundary_70", intPtr(70), "🟢"},
		{"boundary_39", intPtr(39), "🔴"},
		{"zero", intPtr(0), "🔴"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := QualityIndicator(tt.input)
			if got != tt.want {
				t.Errorf("QualityIndicator(%v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSeedHealthIndicator(t *testing.T) {
	tests := []struct {
		seeds int
		want  string
	}{
		{0, "🔴"},
		{5, "🔴"},
		{9, "🔴"},
		{10, "🟡"},
		{50, "🟡"},
		{100, "🟡"},
		{101, "🟢"},
		{1000, "🟢"},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("seeds_%d", tt.seeds), func(t *testing.T) {
			got := SeedHealthIndicator(tt.seeds)
			if got != tt.want {
				t.Errorf("SeedHealthIndicator(%d) = %q, want %q", tt.seeds, got, tt.want)
			}
		})
	}
}

func TestFormatRating(t *testing.T) {
	tests := []struct {
		name  string
		input *string
		want  string
	}{
		{"nil", nil, "-"},
		{"value", strPtr("8.5"), "8.5"},
		{"empty", strPtr(""), ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatRating(tt.input)
			if got != tt.want {
				t.Errorf("FormatRating(%v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestStringOrDash(t *testing.T) {
	s := "hello"
	if got := StringOrDash(&s); got != "hello" {
		t.Errorf("StringOrDash(&hello) = %q, want hello", got)
	}
	if got := StringOrDash(nil); got != "-" {
		t.Errorf("StringOrDash(nil) = %q, want -", got)
	}
}

func TestFormatContentType(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"movie", "Movie"},
		{"Movie", "Movie"},
		{"MOVIE", "Movie"},
		{"show", "Show"},
		{"Show", "Show"},
		{"other", "other"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := FormatContentType(tt.input)
			if got != tt.want {
				t.Errorf("FormatContentType(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestFormatLanguages(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  string
	}{
		{"nil", nil, "-"},
		{"empty", []string{}, "-"},
		{"single", []string{"en"}, "en"},
		{"multiple", []string{"en", "es", "fr"}, "en, es, fr"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatLanguages(tt.input)
			if got != tt.want {
				t.Errorf("FormatLanguages(%v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestFormatSeedRatio(t *testing.T) {
	tests := []struct {
		seeders  int
		leechers int
		want     string
	}{
		{0, 0, "0:0"},
		{10, 0, "10:0"},
		{100, 10, "10:1"},
		{50, 50, "1:1"},
		{1, 3, "0:1"},
		{150, 10, "15:1"},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d_%d", tt.seeders, tt.leechers), func(t *testing.T) {
			got := FormatSeedRatio(tt.seeders, tt.leechers)
			if got != tt.want {
				t.Errorf("FormatSeedRatio(%d, %d) = %q, want %q", tt.seeders, tt.leechers, got, tt.want)
			}
		})
	}
}

func TestFormatTimeAgo(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"invalid", "not-a-date", "not-a-date"},
		{"just_now", now.Add(-10 * time.Second).Format(time.RFC3339), "just now"},
		{"minutes", now.Add(-5 * time.Minute).Format(time.RFC3339), "5m ago"},
		{"hours", now.Add(-3 * time.Hour).Format(time.RFC3339), "3h ago"},
		{"days", now.Add(-7 * 24 * time.Hour).Format(time.RFC3339), "7d ago"},
		{"months", now.Add(-60 * 24 * time.Hour).Format(time.RFC3339), "2mo ago"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatTimeAgo(tt.input)
			if got != tt.want {
				t.Errorf("FormatTimeAgo(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestFormatNumberExtended(t *testing.T) {
	tests := []struct {
		input int
		want  string
	}{
		{-1000, "-1,000"},
		{-5, "-5"},
		{10000, "10,000"},
		{100000, "100,000"},
		{1000000, "1,000,000"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := FormatNumber(tt.input)
			if got != tt.want {
				t.Errorf("FormatNumber(%d) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestTruncateStringEdgeCases(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{"maxLen_1", "hello", 1, "h"},
		{"maxLen_3", "hello", 3, "hel"},
		{"empty", "", 5, ""},
		{"unicode", "こんにちは世界", 5, "こん..."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TruncateString(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("TruncateString(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
}

func TestPtr(t *testing.T) {
	v := 42
	p := Ptr(v)
	if *p != 42 {
		t.Errorf("Ptr(42) = %d, want 42", *p)
	}

	s := "hello"
	sp := Ptr(s)
	if *sp != "hello" {
		t.Errorf("Ptr(hello) = %q, want hello", *sp)
	}
}

func ptr[T any](v T) *T       { return &v }
func intPtr(v int) *int       { return &v }
func strPtr(v string) *string { return &v }

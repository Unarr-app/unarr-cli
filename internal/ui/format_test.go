package ui

import (
	"testing"
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
	if got := FormatContentType("movie"); got != "Movie" {
		t.Errorf("FormatContentType(movie) = %q, want Movie", got)
	}
	if got := FormatContentType("show"); got != "Show" {
		t.Errorf("FormatContentType(show) = %q, want Show", got)
	}
	if got := FormatContentType("other"); got != "other" {
		t.Errorf("FormatContentType(other) = %q, want other", got)
	}
}

func ptr[T any](v T) *T { return &v }
func intPtr(v int) *int { return &v }

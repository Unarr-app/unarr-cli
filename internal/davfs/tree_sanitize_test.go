package davfs

import (
	"strings"
	"testing"
	"time"
)

// TestSanitizeName: a title must reduce to a single SAFE, FLAT path segment — a
// separator, "..", control char, or stray dot/space in a title can never produce
// path traversal or a nested dir when used as a Movies/<title> folder.
func TestSanitizeName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "Normal Title", "Normal Title"},
		{"forward slash becomes space", "Season/Finale", "Season Finale"},
		{"backslash becomes space", `Back\Slash`, "Back Slash"},
		{"dot dot collapses to empty", "..", ""},
		{"leading dot trimmed", ".hidden", "hidden"},
		{"trailing dot trimmed (illegal in some WebDAV clients)", "Movie.", "Movie"},
		{"leading/trailing dots and spaces trimmed, internal collapsed", " . weird . ", "weird"},
		{"control chars dropped", "a\x00b\x1fc\x7fd", "abcd"},
		{"internal whitespace collapsed", "multi   space", "multi space"},
		{"traversal attempt flattened", "../../etc/passwd", "etc passwd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeName(tt.in); got != tt.want {
				t.Errorf("sanitizeName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}

	// The core safety invariant across a batch of hostile titles: never a path
	// separator, never a leading dot, never a bare traversal token.
	for _, in := range []string{"../secret", `..\secret`, "a/b/c", "...", "  ..  ", "/etc", `\\server\share`} {
		got := sanitizeName(in)
		if strings.ContainsAny(got, `/\`) {
			t.Errorf("sanitizeName(%q) = %q still contains a path separator", in, got)
		}
		if strings.HasPrefix(got, ".") {
			t.Errorf("sanitizeName(%q) = %q starts with a dot (traversal/hidden risk)", in, got)
		}
		if got == ".." {
			t.Errorf("sanitizeName(%q) = %q is a bare traversal token", in, got)
		}
	}
}

// TestParseModTime: a valid RFC3339 timestamp parses; a malformed/empty one falls
// back cleanly to the zero time instead of propagating a parse error into the tree.
func TestParseModTime(t *testing.T) {
	want := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)
	if got := parseModTime("2021-03-04T05:06:07Z"); !got.Equal(want) {
		t.Errorf("parseModTime(valid) = %v, want %v", got, want)
	}
	for _, bad := range []string{"", "not-a-time", "2021-13-99T99:99:99Z", "1614834367"} {
		if got := parseModTime(bad); !got.IsZero() {
			t.Errorf("parseModTime(%q) = %v, want the zero time", bad, got)
		}
	}
}

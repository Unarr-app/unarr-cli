package engine

import "testing"

func TestIsWithinDir(t *testing.T) {
	tests := []struct {
		base   string
		target string
		want   bool
	}{
		{"/data", "/data/file.txt", true},
		{"/data", "/data/sub/file.txt", true},
		{"/data", "/data", true},
		{"/data", "/data/../etc/passwd", false},
		{"/data", "/etc/passwd", false},
		{"/data", "/", false},
		{"/data", "/datafoo", false}, // not a child, just a prefix
	}

	for _, tt := range tests {
		got := isWithinDir(tt.base, tt.target)
		if got != tt.want {
			t.Errorf("isWithinDir(%q, %q) = %v, want %v", tt.base, tt.target, got, tt.want)
		}
	}
}

func TestSafePath(t *testing.T) {
	tests := []struct {
		base      string
		untrusted string
		wantErr   bool
	}{
		{"/data", "movie.mkv", false},
		{"/data", "sub/file.mkv", false},
		{"/data", "../etc/passwd", true},
		{"/data", "../../root/.ssh", true},
		{"/data", "normal/../still-ok", false},
	}

	for _, tt := range tests {
		_, err := safePath(tt.base, tt.untrusted)
		if (err != nil) != tt.wantErr {
			t.Errorf("safePath(%q, %q) error = %v, wantErr %v", tt.base, tt.untrusted, err, tt.wantErr)
		}
	}
}

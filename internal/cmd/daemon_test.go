package cmd

import (
	"testing"
)

func TestIsAllowedStreamPath(t *testing.T) {
	tests := []struct {
		name        string
		filePath    string
		allowedDirs []string
		want        bool
	}{
		{
			name:        "path inside download dir",
			filePath:    "/downloads/movie.mkv",
			allowedDirs: []string{"/downloads"},
			want:        true,
		},
		{
			name:        "path inside subdirectory",
			filePath:    "/downloads/sub/movie.mkv",
			allowedDirs: []string{"/downloads"},
			want:        true,
		},
		{
			name:        "path traversal attempt",
			filePath:    "/downloads/../etc/passwd",
			allowedDirs: []string{"/downloads"},
			want:        false,
		},
		{
			name:        "path outside all allowed dirs",
			filePath:    "/etc/passwd",
			allowedDirs: []string{"/downloads", "/movies"},
			want:        false,
		},
		{
			name:        "path inside second allowed dir",
			filePath:    "/movies/action/movie.mkv",
			allowedDirs: []string{"/downloads", "/movies"},
			want:        true,
		},
		{
			name:        "empty allowed dirs",
			filePath:    "/downloads/movie.mkv",
			allowedDirs: []string{"", ""},
			want:        false,
		},
		{
			name:        "path equals allowed dir exactly",
			filePath:    "/downloads",
			allowedDirs: []string{"/downloads"},
			want:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isAllowedStreamPath(tt.filePath, tt.allowedDirs...)
			if got != tt.want {
				t.Errorf("isAllowedStreamPath(%q, %v) = %v, want %v",
					tt.filePath, tt.allowedDirs, got, tt.want)
			}
		})
	}
}

func TestFormatSpeedLog(t *testing.T) {
	tests := []struct {
		bps  int64
		want string
	}{
		{0, "0 B/s"},
		{500, "500 B/s"},
		{1023, "1023 B/s"},
		{1024, "1 KB/s"},
		{10240, "10 KB/s"},
		{1048576, "1.0 MB/s"},
		{5242880, "5.0 MB/s"},
		{1073741824, "1.0 GB/s"},
		{2147483648, "2.0 GB/s"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := formatSpeedLog(tt.bps)
			if got != tt.want {
				t.Errorf("formatSpeedLog(%d) = %q, want %q", tt.bps, got, tt.want)
			}
		})
	}
}

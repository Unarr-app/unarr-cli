package cmd

import (
	"os"
	"strings"
	"testing"
)

func TestExpandHome(t *testing.T) {
	home, _ := os.UserHomeDir()

	tests := []struct {
		input string
		want  string
	}{
		{"~/Documents", home + "/Documents"},
		{"~/", home},
		{"/absolute/path", "/absolute/path"},
		{"relative/path", "relative/path"},
		{"", ""},
		{"~notexpanded", "~notexpanded"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := expandHome(tt.input)
			if got != tt.want {
				t.Errorf("expandHome(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestDefaultDownloadDir(t *testing.T) {
	dir := defaultDownloadDir()
	if dir == "" {
		t.Error("defaultDownloadDir() returned empty string")
	}
	home, _ := os.UserHomeDir()
	if !strings.HasPrefix(dir, home) {
		t.Errorf("defaultDownloadDir() = %q, expected to start with home dir %q", dir, home)
	}
}

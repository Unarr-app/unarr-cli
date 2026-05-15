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

func TestIsSafeBrowserURL(t *testing.T) {
	good := []string{
		"http://localhost:3000",
		"https://torrentclaw.com/some/path?q=1",
	}
	bad := []string{
		"--help",
		"-version",
		"file:///etc/passwd",
		"javascript:alert(1)",
		"data:text/html,foo",
		"ftp://example.com",
		"",
	}
	for _, u := range good {
		if !isSafeBrowserURL(u) {
			t.Errorf("isSafeBrowserURL(%q) = false, want true", u)
		}
	}
	for _, u := range bad {
		if isSafeBrowserURL(u) {
			t.Errorf("isSafeBrowserURL(%q) = true, want false", u)
		}
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

package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

// TestIsAllowedStreamPathResolved covers the symlink-hardened gate used by the
// WebDAV export: a symlink living inside an allowed root but pointing OUTSIDE it
// must be denied (the lexical check alone would let os.Open follow it out),
// while a plain file — and a library legitimately mounted THROUGH a symlinked
// root — must still be allowed. A missing file is fail-closed.
func TestIsAllowedStreamPathResolved(t *testing.T) {
	// Real dir that some symlinks will escape to.
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	realFile := filepath.Join(root, "movie.mkv")
	if err := os.WriteFile(realFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A symlink inside the root that escapes to a file outside it.
	escape := filepath.Join(root, "escape.mkv")
	if err := os.Symlink(secret, escape); err != nil {
		t.Fatal(err)
	}

	t.Run("plain file inside root allowed", func(t *testing.T) {
		if !isAllowedStreamPathResolved(realFile, root) {
			t.Errorf("plain file under root should be allowed")
		}
	})

	t.Run("symlink escaping root denied", func(t *testing.T) {
		if isAllowedStreamPathResolved(escape, root) {
			t.Errorf("symlink pointing outside the root must be denied")
		}
	})

	t.Run("root reached through a symlink still allowed", func(t *testing.T) {
		// linkRoot → root; a file addressed via the symlinked root must resolve
		// back inside root and stay allowed (both sides resolved consistently).
		linkRoot := filepath.Join(t.TempDir(), "libroot")
		if err := os.Symlink(root, linkRoot); err != nil {
			t.Fatal(err)
		}
		via := filepath.Join(linkRoot, "movie.mkv")
		if !isAllowedStreamPathResolved(via, linkRoot) {
			t.Errorf("file under a symlinked-but-legit root should be allowed")
		}
	})

	t.Run("missing file fail-closed", func(t *testing.T) {
		if isAllowedStreamPathResolved(filepath.Join(root, "nope.mkv"), root) {
			t.Errorf("a nonexistent path must be denied (fail-closed)")
		}
	})
}

// TestWebDAVOverUPnPExposed: the advisory fires ONLY when the read-only WebDAV
// export and UPnP WAN port-mapping are BOTH on (that combination exposes /dav/
// over cleartext to the public internet); any other combination is quiet.
func TestWebDAVOverUPnPExposed(t *testing.T) {
	cases := []struct {
		webdav, upnp, want bool
	}{
		{true, true, true},
		{true, false, false},
		{false, true, false},
		{false, false, false},
	}
	for _, c := range cases {
		cfg := config.Config{Download: config.DownloadConfig{WebDAVEnabled: c.webdav, EnableUPnP: c.upnp}}
		if got := webDAVOverUPnPExposed(cfg); got != c.want {
			t.Errorf("webDAVOverUPnPExposed(webdav=%v upnp=%v) = %v, want %v", c.webdav, c.upnp, got, c.want)
		}
	}
}

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

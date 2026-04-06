package mediainfo

import (
	"archive/zip"
	"bytes"
	"runtime"
	"testing"
)

func TestFFprobePlatformKey(t *testing.T) {
	key, err := ffprobePlatformKey()
	if err != nil {
		// Only error on unsupported platforms
		if runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
			return // expected to fail on unsupported platforms
		}
		t.Fatalf("ffprobePlatformKey: %v", err)
	}
	if key == "" {
		t.Error("platform key should not be empty")
	}

	// Verify format based on current platform
	switch runtime.GOOS {
	case "linux":
		switch runtime.GOARCH {
		case "amd64":
			if key != "linux-64" {
				t.Errorf("key = %q, want linux-64", key)
			}
		case "arm64":
			if key != "linux-arm64" {
				t.Errorf("key = %q, want linux-arm64", key)
			}
		}
	case "darwin":
		if key != "osx-64" {
			t.Errorf("key = %q, want osx-64", key)
		}
	case "windows":
		if runtime.GOARCH == "amd64" && key != "windows-64" {
			t.Errorf("key = %q, want windows-64", key)
		}
	}
}

func TestFFprobeCacheDir(t *testing.T) {
	dir, err := FFprobeCacheDir()
	if err != nil {
		t.Fatalf("FFprobeCacheDir: %v", err)
	}
	if dir == "" {
		t.Error("cache dir should not be empty")
	}
}

func TestFFprobeCachePath(t *testing.T) {
	path, err := FFprobeCachePath()
	if err != nil {
		t.Fatalf("FFprobeCachePath: %v", err)
	}
	if path == "" {
		t.Error("cache path should not be empty")
	}
}

func TestExtractFromZip(t *testing.T) {
	// Create a zip in memory containing a "ffprobe" file
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)

	content := []byte("fake ffprobe binary content")
	f, err := w.Create("ffprobe")
	if err != nil {
		t.Fatal(err)
	}
	f.Write(content)

	// Add another file to make it realistic
	readme, _ := w.Create("README.md")
	readme.Write([]byte("some readme"))

	w.Close()

	data, err := extractFromZip(buf.Bytes(), "ffprobe")
	if err != nil {
		t.Fatalf("extractFromZip: %v", err)
	}
	if string(data) != string(content) {
		t.Errorf("content = %q, want %q", string(data), string(content))
	}
}

func TestExtractFromZipNotFound(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, _ := w.Create("other-file.txt")
	f.Write([]byte("data"))
	w.Close()

	_, err := extractFromZip(buf.Bytes(), "ffprobe")
	if err == nil {
		t.Error("expected error when target not in zip")
	}
}

func TestExtractFromZipInvalidData(t *testing.T) {
	_, err := extractFromZip([]byte("not a zip"), "ffprobe")
	if err == nil {
		t.Error("expected error for invalid zip data")
	}
}

func TestExtractFromZipWindowsExe(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)

	content := []byte("fake exe")
	f, _ := w.Create("bin/ffprobe.exe")
	f.Write(content)
	w.Close()

	data, err := extractFromZip(buf.Bytes(), "ffprobe.exe")
	if err != nil {
		t.Fatalf("extractFromZip: %v", err)
	}
	if string(data) != string(content) {
		t.Errorf("content mismatch")
	}
}

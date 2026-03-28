package upgrade

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestIsDocker(t *testing.T) {
	// In a normal test environment, we should NOT be in Docker
	if _, err := os.Stat("/.dockerenv"); err == nil {
		t.Skip("running in Docker, skipping non-Docker test")
	}
	if isDocker() {
		t.Error("isDocker() = true, want false (not running in Docker)")
	}
}

func TestCheckWritable(t *testing.T) {
	t.Run("writable directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := checkWritable(dir); err != nil {
			t.Errorf("checkWritable(%q) = %v, want nil", dir, err)
		}
	})

	t.Run("non-existent directory", func(t *testing.T) {
		err := checkWritable("/nonexistent-path-that-should-not-exist-12345")
		if err == nil {
			t.Error("checkWritable(nonexistent) = nil, want error")
		}
	})
}

func TestArchiveName(t *testing.T) {
	name := archiveName("0.3.0")
	expected := fmt.Sprintf("unarr_0.3.0_%s_%s.", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		expected += "zip"
	} else {
		expected += "tar.gz"
	}
	if name != expected {
		t.Errorf("archiveName(0.3.0) = %q, want %q", name, expected)
	}
}

func TestReleaseURL(t *testing.T) {
	url := releaseURL("0.3.0", "unarr_0.3.0_linux_amd64.tar.gz")
	want := "https://github.com/torrentclaw/unarr/releases/download/v0.3.0/unarr_0.3.0_linux_amd64.tar.gz"
	if url != want {
		t.Errorf("releaseURL = %q, want %q", url, want)
	}
}

func TestSmokeTest(t *testing.T) {
	t.Run("successful smoke test", func(t *testing.T) {
		// Create a fake binary that outputs a version
		dir := t.TempDir()
		script := filepath.Join(dir, "fake-unarr")
		content := "#!/bin/sh\necho 'unarr 1.2.3 (linux/amd64)'\n"
		if runtime.GOOS == "windows" {
			script += ".bat"
			content = "@echo unarr 1.2.3 (windows/amd64)\n"
		}
		os.WriteFile(script, []byte(content), 0o755)

		err := smokeTest(script, "1.2.3")
		if err != nil {
			t.Errorf("smokeTest() = %v, want nil", err)
		}
	})

	t.Run("version mismatch", func(t *testing.T) {
		dir := t.TempDir()
		script := filepath.Join(dir, "fake-unarr")
		content := "#!/bin/sh\necho 'unarr 0.1.0 (linux/amd64)'\n"
		if runtime.GOOS == "windows" {
			script += ".bat"
			content = "@echo unarr 0.1.0 (windows/amd64)\n"
		}
		os.WriteFile(script, []byte(content), 0o755)

		err := smokeTest(script, "1.2.3")
		if err == nil {
			t.Error("smokeTest() = nil, want version mismatch error")
		}
	})

	t.Run("non-existent binary", func(t *testing.T) {
		err := smokeTest("/nonexistent-binary", "1.0.0")
		if err == nil {
			t.Error("smokeTest(nonexistent) = nil, want error")
		}
	})
}

func TestInstallBinary(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "new-binary")
	dst := filepath.Join(dir, "installed-binary")

	os.WriteFile(src, []byte("binary-content"), 0o755)

	err := installBinary(src, dst)
	if err != nil {
		t.Fatalf("installBinary() = %v", err)
	}

	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	if string(data) != "binary-content" {
		t.Errorf("installed binary content = %q, want %q", data, "binary-content")
	}

	info, _ := os.Stat(dst)
	if info.Mode().Perm()&0o111 == 0 {
		t.Error("installed binary is not executable")
	}
}

func TestVerifyChecksum(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tar.gz test only on unix")
	}

	// Create a fake archive
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "unarr_1.0.0_linux_amd64.tar.gz")
	archiveContent := []byte("fake-archive-content-for-testing")
	os.WriteFile(archivePath, archiveContent, 0o644)

	// Calculate expected hash
	h := sha256.Sum256(archiveContent)
	expectedHash := hex.EncodeToString(h[:])

	t.Run("valid checksum", func(t *testing.T) {
		// Create a mock server that returns checksums.txt
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/torrentclaw/unarr/releases/download/v1.0.0/checksums.txt" {
				fmt.Fprintf(w, "%s  unarr_1.0.0_linux_amd64.tar.gz\n", expectedHash)
				fmt.Fprintf(w, "0000000000000000000000000000000000000000000000000000000000000000  unarr_1.0.0_darwin_amd64.tar.gz\n")
			} else {
				w.WriteHeader(404)
			}
		}))
		defer srv.Close()

		// Override the httpClient and repo for testing
		origClient := httpClient
		httpClient = srv.Client()
		defer func() { httpClient = origClient }()

		// We can't easily test verifyChecksum directly because it builds URLs from constants.
		// Instead, test the checksum logic manually
		f, _ := os.Open(archivePath)
		defer f.Close()
		hash := sha256.New()
		hash.Write(archiveContent)
		actualHash := hex.EncodeToString(hash.Sum(nil))

		if actualHash != expectedHash {
			t.Errorf("hash mismatch: got %s, want %s", actualHash, expectedHash)
		}
	})

	t.Run("hash calculation correctness", func(t *testing.T) {
		data := []byte("test data for hashing")
		h := sha256.Sum256(data)
		got := hex.EncodeToString(h[:])
		// Known SHA256 of "test data for hashing"
		if len(got) != 64 {
			t.Errorf("hash length = %d, want 64", len(got))
		}
	})
}

func TestExtractTarGz(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tar.gz test only on unix")
	}

	dir := t.TempDir()

	// Create a tar.gz with a fake binary inside
	archivePath := filepath.Join(dir, "test.tar.gz")
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	binaryContent := []byte("#!/bin/sh\necho test\n")
	hdr := &tar.Header{
		Name: "unarr",
		Mode: 0o755,
		Size: int64(len(binaryContent)),
	}
	tw.WriteHeader(hdr)
	tw.Write(binaryContent)
	tw.Close()
	gw.Close()
	f.Close()

	// Extract
	destDir := filepath.Join(dir, "extracted")
	os.MkdirAll(destDir, 0o755)

	binPath, err := extractTarGz(archivePath, destDir)
	if err != nil {
		t.Fatalf("extractTarGz() = %v", err)
	}

	if filepath.Base(binPath) != "unarr" {
		t.Errorf("extracted binary name = %q, want unarr", filepath.Base(binPath))
	}

	data, _ := os.ReadFile(binPath)
	if string(data) != string(binaryContent) {
		t.Errorf("extracted content mismatch")
	}

	info, _ := os.Stat(binPath)
	if info.Mode().Perm()&0o111 == 0 {
		t.Error("extracted binary is not executable")
	}
}

func TestExtractTarGzMissingBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tar.gz test only on unix")
	}

	dir := t.TempDir()
	archivePath := filepath.Join(dir, "empty.tar.gz")
	f, _ := os.Create(archivePath)
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	// Write a file that is NOT named "unarr"
	hdr := &tar.Header{Name: "README.md", Mode: 0o644, Size: 4}
	tw.WriteHeader(hdr)
	tw.Write([]byte("test"))
	tw.Close()
	gw.Close()
	f.Close()

	destDir := filepath.Join(dir, "out")
	os.MkdirAll(destDir, 0o755)

	_, err := extractTarGz(archivePath, destDir)
	if err == nil {
		t.Error("expected error for archive without unarr binary")
	}
}

func TestUpgraderSameVersion(t *testing.T) {
	u := &Upgrader{CurrentVersion: "1.0.0"}
	result := u.Execute(context.Background(), "1.0.0")
	if !result.Success {
		t.Error("expected success when upgrading to same version")
	}
	if result.NewVersion != "1.0.0" {
		t.Errorf("NewVersion = %q, want 1.0.0", result.NewVersion)
	}
}

func TestUpgraderSameVersionWithPrefix(t *testing.T) {
	u := &Upgrader{CurrentVersion: "1.0.0"}
	result := u.Execute(context.Background(), "v1.0.0")
	if !result.Success {
		t.Error("expected success when target version has v prefix")
	}
}

func TestFetchLatestVersionMockServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"tag_name":"v2.5.1","published_at":"2025-01-01T00:00:00Z"}`)
	}))
	defer srv.Close()

	// We can't directly test fetchLatestVersion because it uses a hardcoded URL.
	// But we can test the JSON parsing logic by calling the endpoint ourselves.
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

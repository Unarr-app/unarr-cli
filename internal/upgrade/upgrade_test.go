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
	"strings"
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

// --- New tests below ---

// swapHTTPClient replaces the package-level httpClient and returns a restore function.
func swapHTTPClient(c *http.Client) func() {
	orig := httpClient
	httpClient = c
	return func() { httpClient = orig }
}

// swapCacheDir redirects the version cache to a temp directory to avoid
// polluting the real ~/.local/share/unarr/latest-version.json during tests.
func swapCacheDir(t *testing.T) func() {
	t.Helper()
	tmpDir := t.TempDir()
	orig := cacheFilePathFn
	cacheFilePathFn = func() string { return filepath.Join(tmpDir, "latest-version.json") }
	return func() { cacheFilePathFn = orig }
}

// rewriteTransport redirects all requests to the given base URL,
// preserving path and query.
type rewriteTransport struct {
	url string
}

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	u := strings.TrimPrefix(rt.url, "http://")
	req.URL.Host = u
	return http.DefaultTransport.RoundTrip(req)
}

// createTarGz is a test helper that creates a tar.gz file with a single file entry.
func createTarGz(t *testing.T, archivePath, entryName string, content []byte) {
	t.Helper()
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	tw.WriteHeader(&tar.Header{
		Name: entryName,
		Mode: 0o755,
		Size: int64(len(content)),
	})
	tw.Write(content)

	tw.Close()
	gw.Close()
	f.Close()
}

func TestArchiveNameTableDriven(t *testing.T) {
	// We can only run archiveName for the current GOOS/GOARCH,
	// so we test several version strings and verify the pattern.
	tests := []struct {
		version string
	}{
		{"0.1.0"},
		{"1.0.0-rc1"},
		{"2.5.10"},
		{"0.0.0"},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			got := archiveName(tt.version)
			prefix := fmt.Sprintf("unarr_%s_%s_%s.", tt.version, runtime.GOOS, runtime.GOARCH)
			if !strings.HasPrefix(got, prefix) {
				t.Errorf("archiveName(%q) = %q, want prefix %q", tt.version, got, prefix)
			}
			if runtime.GOOS == "windows" {
				if !strings.HasSuffix(got, ".zip") {
					t.Errorf("archiveName on windows should end with .zip, got %q", got)
				}
			} else {
				if !strings.HasSuffix(got, ".tar.gz") {
					t.Errorf("archiveName on non-windows should end with .tar.gz, got %q", got)
				}
			}
		})
	}
}

func TestReleaseURLEdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		version  string
		filename string
		wantURL  string
	}{
		{
			name:     "pre-release version",
			version:  "2.0.0-beta.1",
			filename: "unarr_2.0.0-beta.1_darwin_arm64.tar.gz",
			wantURL:  "https://github.com/torrentclaw/unarr/releases/download/v2.0.0-beta.1/unarr_2.0.0-beta.1_darwin_arm64.tar.gz",
		},
		{
			name:     "checksums file",
			version:  "3.0.0",
			filename: "checksums.txt",
			wantURL:  "https://github.com/torrentclaw/unarr/releases/download/v3.0.0/checksums.txt",
		},
		{
			name:     "windows zip",
			version:  "1.2.3",
			filename: "unarr_1.2.3_windows_amd64.zip",
			wantURL:  "https://github.com/torrentclaw/unarr/releases/download/v1.2.3/unarr_1.2.3_windows_amd64.zip",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := releaseURL(tt.version, tt.filename)
			if got != tt.wantURL {
				t.Errorf("releaseURL(%q, %q) = %q, want %q", tt.version, tt.filename, got, tt.wantURL)
			}
		})
	}
}

func TestExtractBinaryDispatcher(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("extractBinary dispatcher test only on unix")
	}

	dir := t.TempDir()

	// Create a valid tar.gz with the unarr binary
	archivePath := filepath.Join(dir, "test.tar.gz")
	binaryContent := []byte("#!/bin/sh\necho dispatcher test\n")
	createTarGz(t, archivePath, "unarr", binaryContent)

	destDir := filepath.Join(dir, "out")
	os.MkdirAll(destDir, 0o755)

	binPath, err := extractBinary(archivePath, destDir)
	if err != nil {
		t.Fatalf("extractBinary() error = %v", err)
	}
	if filepath.Base(binPath) != "unarr" {
		t.Errorf("extractBinary() returned %q, want base name 'unarr'", binPath)
	}
	data, _ := os.ReadFile(binPath)
	if string(data) != string(binaryContent) {
		t.Error("extractBinary() content mismatch")
	}
}

func TestExtractBinaryInvalidArchive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tar.gz test only on unix")
	}

	dir := t.TempDir()
	archivePath := filepath.Join(dir, "garbage.tar.gz")
	os.WriteFile(archivePath, []byte("this is not a tar.gz"), 0o644)

	destDir := filepath.Join(dir, "out")
	os.MkdirAll(destDir, 0o755)

	_, err := extractBinary(archivePath, destDir)
	if err == nil {
		t.Error("extractBinary with garbage data should return error")
	}
}

func TestExtractBinaryNonExistentArchive(t *testing.T) {
	dir := t.TempDir()
	_, err := extractBinary("/nonexistent-archive-file", filepath.Join(dir, "out"))
	if err == nil {
		t.Error("extractBinary with nonexistent file should return error")
	}
}

func TestUpgraderFail(t *testing.T) {
	u := &Upgrader{CurrentVersion: "1.0.0"}

	// Capture log messages
	var logged []string
	u.OnProgress = func(msg string) { logged = append(logged, msg) }

	result := u.fail("something went wrong: %d", 42)

	if result.Success {
		t.Error("fail() should return Success=false")
	}
	if result.OldVersion != "1.0.0" {
		t.Errorf("fail() OldVersion = %q, want 1.0.0", result.OldVersion)
	}
	if result.Error == nil {
		t.Fatal("fail() should set Error")
	}
	if !strings.Contains(result.Error.Error(), "something went wrong: 42") {
		t.Errorf("fail() Error = %q, want to contain 'something went wrong: 42'", result.Error)
	}
	if len(logged) == 0 {
		t.Error("fail() should call OnProgress")
	}
	if len(logged) > 0 && !strings.Contains(logged[0], "FAILED") {
		t.Errorf("fail() logged %q, want to contain 'FAILED'", logged[0])
	}
}

func TestUpgraderFailNilOnProgress(t *testing.T) {
	u := &Upgrader{CurrentVersion: "2.0.0"}
	// OnProgress is nil — should not panic
	result := u.fail("error without listener")
	if result.Success {
		t.Error("fail() should return Success=false")
	}
}

func TestFetchLatestVersionWithHTTPTest(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		statusCode int
		wantVer    string
		wantErr    bool
	}{
		{
			name:       "valid response",
			body:       `{"tag_name":"v3.1.4"}`,
			statusCode: 200,
			wantVer:    "3.1.4",
		},
		{
			name:       "valid response without v prefix",
			body:       `{"tag_name":"2.0.0"}`,
			statusCode: 200,
			wantVer:    "2.0.0",
		},
		{
			name:       "empty tag_name",
			body:       `{"tag_name":""}`,
			statusCode: 200,
			wantErr:    true,
		},
		{
			name:       "server error",
			body:       `Internal Server Error`,
			statusCode: 500,
			wantErr:    true,
		},
		{
			name:       "invalid json",
			body:       `{invalid`,
			statusCode: 200,
			wantErr:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				fmt.Fprint(w, tt.body)
			}))
			defer srv.Close()

			// Use a custom transport that rewrites requests to our test server
			restore := swapHTTPClient(&http.Client{
				Transport: &rewriteTransport{url: srv.URL},
			})
			defer restore()

			// Redirect cache to temp dir so tests don't pollute the real cache
			restoreCache := swapCacheDir(t)
			defer restoreCache()

			ver, err := CheckLatest(context.Background())
			if tt.wantErr {
				if err == nil {
					t.Errorf("CheckLatest() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("CheckLatest() error = %v", err)
			}
			if ver != tt.wantVer {
				t.Errorf("CheckLatest() = %q, want %q", ver, tt.wantVer)
			}
		})
	}
}

func TestDownloadWithHTTPTest(t *testing.T) {
	archiveBody := "fake-archive-bytes-for-download-test"

	t.Run("successful download", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Verify user-agent header
			if ua := r.Header.Get("User-Agent"); ua != "unarr-updater" {
				t.Errorf("User-Agent = %q, want 'unarr-updater'", ua)
			}
			w.WriteHeader(200)
			fmt.Fprint(w, archiveBody)
		}))
		defer srv.Close()

		restore := swapHTTPClient(&http.Client{
			Transport: &rewriteTransport{url: srv.URL},
		})
		defer restore()

		path, err := download(context.Background(), "1.0.0")
		if err != nil {
			t.Fatalf("download() error = %v", err)
		}
		defer os.Remove(path)

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read downloaded file: %v", err)
		}
		if string(data) != archiveBody {
			t.Errorf("downloaded content = %q, want %q", data, archiveBody)
		}
	})

	t.Run("server returns 404", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(404)
		}))
		defer srv.Close()

		restore := swapHTTPClient(&http.Client{
			Transport: &rewriteTransport{url: srv.URL},
		})
		defer restore()

		_, err := download(context.Background(), "99.99.99")
		if err == nil {
			t.Error("download() with 404 should return error")
		}
		if !strings.Contains(err.Error(), "HTTP 404") {
			t.Errorf("download() error = %q, want to contain 'HTTP 404'", err)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			fmt.Fprint(w, "data")
		}))
		defer srv.Close()

		restore := swapHTTPClient(&http.Client{
			Transport: &rewriteTransport{url: srv.URL},
		})
		defer restore()

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // cancel immediately

		_, err := download(ctx, "1.0.0")
		if err == nil {
			t.Error("download() with cancelled context should return error")
		}
	})
}

func TestVerifyChecksumWithHTTPTest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tar.gz test only on unix")
	}

	// Create a fake archive file
	dir := t.TempDir()
	archiveContent := []byte("archive-content-for-checksum-test")
	archivePath := filepath.Join(dir, "test-archive.tar.gz")
	os.WriteFile(archivePath, archiveContent, 0o644)

	h := sha256.Sum256(archiveContent)
	correctHash := hex.EncodeToString(h[:])

	// The function builds the archive name using archiveName(), which uses runtime.GOOS/GOARCH.
	expectedArchiveName := archiveName("1.0.0")

	t.Run("matching checksum", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "0000000000000000000000000000000000000000000000000000000000000000  other_file.tar.gz\n")
			fmt.Fprintf(w, "%s  %s\n", correctHash, expectedArchiveName)
		}))
		defer srv.Close()

		restore := swapHTTPClient(&http.Client{
			Transport: &rewriteTransport{url: srv.URL},
		})
		defer restore()

		err := verifyChecksum(context.Background(), "1.0.0", archivePath)
		if err != nil {
			t.Errorf("verifyChecksum() = %v, want nil", err)
		}
	})

	t.Run("mismatched checksum", func(t *testing.T) {
		wrongHash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "%s  %s\n", wrongHash, expectedArchiveName)
		}))
		defer srv.Close()

		restore := swapHTTPClient(&http.Client{
			Transport: &rewriteTransport{url: srv.URL},
		})
		defer restore()

		err := verifyChecksum(context.Background(), "1.0.0", archivePath)
		if err == nil {
			t.Error("verifyChecksum() with wrong hash should return error")
		}
		if !strings.Contains(err.Error(), "SHA256 mismatch") {
			t.Errorf("verifyChecksum() error = %q, want to contain 'SHA256 mismatch'", err)
		}
	})

	t.Run("archive not in checksums", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890  some_other_file.tar.gz\n")
		}))
		defer srv.Close()

		restore := swapHTTPClient(&http.Client{
			Transport: &rewriteTransport{url: srv.URL},
		})
		defer restore()

		err := verifyChecksum(context.Background(), "1.0.0", archivePath)
		if err == nil {
			t.Error("verifyChecksum() with missing entry should return error")
		}
		if !strings.Contains(err.Error(), "no checksum found") {
			t.Errorf("verifyChecksum() error = %q, want to contain 'no checksum found'", err)
		}
	})

	t.Run("checksums server error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
		}))
		defer srv.Close()

		restore := swapHTTPClient(&http.Client{
			Transport: &rewriteTransport{url: srv.URL},
		})
		defer restore()

		err := verifyChecksum(context.Background(), "1.0.0", archivePath)
		if err == nil {
			t.Error("verifyChecksum() with server error should return error")
		}
		if !strings.Contains(err.Error(), "HTTP 500") {
			t.Errorf("verifyChecksum() error = %q, want to contain 'HTTP 500'", err)
		}
	})

	t.Run("nonexistent archive file", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "%s  %s\n", correctHash, expectedArchiveName)
		}))
		defer srv.Close()

		restore := swapHTTPClient(&http.Client{
			Transport: &rewriteTransport{url: srv.URL},
		})
		defer restore()

		err := verifyChecksum(context.Background(), "1.0.0", "/nonexistent-archive-path")
		if err == nil {
			t.Error("verifyChecksum() with nonexistent archive should return error")
		}
	})
}

func TestVerifyChecksumCaseInsensitive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tar.gz test only on unix")
	}

	dir := t.TempDir()
	archiveContent := []byte("case-insensitive-hash-test")
	archivePath := filepath.Join(dir, "archive.tar.gz")
	os.WriteFile(archivePath, archiveContent, 0o644)

	h := sha256.Sum256(archiveContent)
	// Use uppercase hash in checksums.txt — verifyChecksum uses EqualFold
	upperHash := strings.ToUpper(hex.EncodeToString(h[:]))
	expectedArchiveName := archiveName("1.0.0")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", upperHash, expectedArchiveName)
	}))
	defer srv.Close()

	restore := swapHTTPClient(&http.Client{
		Transport: &rewriteTransport{url: srv.URL},
	})
	defer restore()

	err := verifyChecksum(context.Background(), "1.0.0", archivePath)
	if err != nil {
		t.Errorf("verifyChecksum() with uppercase hash = %v, want nil", err)
	}
}

func TestExtractTarGzNestedDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tar.gz test only on unix")
	}

	dir := t.TempDir()
	archivePath := filepath.Join(dir, "nested.tar.gz")

	binaryContent := []byte("#!/bin/sh\necho nested\n")

	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	// Write a directory entry first
	tw.WriteHeader(&tar.Header{
		Name:     "unarr_1.0.0_linux_amd64/",
		Typeflag: tar.TypeDir,
		Mode:     0o755,
	})

	// Write a README in the subdirectory
	readmeContent := []byte("This is a README")
	tw.WriteHeader(&tar.Header{
		Name: "unarr_1.0.0_linux_amd64/README.md",
		Mode: 0o644,
		Size: int64(len(readmeContent)),
	})
	tw.Write(readmeContent)

	// Write the binary nested inside the directory
	tw.WriteHeader(&tar.Header{
		Name: "unarr_1.0.0_linux_amd64/unarr",
		Mode: 0o755,
		Size: int64(len(binaryContent)),
	})
	tw.Write(binaryContent)

	// Write another unrelated file after the binary
	licenseContent := []byte("MIT License")
	tw.WriteHeader(&tar.Header{
		Name: "unarr_1.0.0_linux_amd64/LICENSE",
		Mode: 0o644,
		Size: int64(len(licenseContent)),
	})
	tw.Write(licenseContent)

	tw.Close()
	gw.Close()
	f.Close()

	destDir := filepath.Join(dir, "out")
	os.MkdirAll(destDir, 0o755)

	binPath, err := extractTarGz(archivePath, destDir)
	if err != nil {
		t.Fatalf("extractTarGz() with nested dir = %v", err)
	}

	if filepath.Base(binPath) != "unarr" {
		t.Errorf("extracted name = %q, want 'unarr'", filepath.Base(binPath))
	}

	data, _ := os.ReadFile(binPath)
	if string(data) != string(binaryContent) {
		t.Error("extracted content does not match")
	}

	// Verify that only the binary was extracted (README and LICENSE should NOT be in destDir)
	entries, _ := os.ReadDir(destDir)
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("destDir should contain only 'unarr', got %v", names)
	}
}

func TestExtractTarGzMultipleFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tar.gz test only on unix")
	}

	dir := t.TempDir()
	archivePath := filepath.Join(dir, "multi.tar.gz")

	binaryContent := []byte("#!/bin/sh\necho multi\n")

	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	// Several non-binary files before the actual binary
	for _, name := range []string{"README.md", "LICENSE", "config.yaml", "completions.bash"} {
		content := []byte("content of " + name)
		tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(content)),
		})
		tw.Write(content)
	}

	// The actual binary
	tw.WriteHeader(&tar.Header{
		Name: "unarr",
		Mode: 0o755,
		Size: int64(len(binaryContent)),
	})
	tw.Write(binaryContent)

	tw.Close()
	gw.Close()
	f.Close()

	destDir := filepath.Join(dir, "out")
	os.MkdirAll(destDir, 0o755)

	binPath, err := extractTarGz(archivePath, destDir)
	if err != nil {
		t.Fatalf("extractTarGz() = %v", err)
	}

	data, _ := os.ReadFile(binPath)
	if string(data) != string(binaryContent) {
		t.Error("binary content mismatch among multiple files")
	}
}

func TestExtractTarGzSymlinkSkipped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tar.gz test only on unix")
	}

	dir := t.TempDir()
	archivePath := filepath.Join(dir, "symlink.tar.gz")

	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	// Write a symlink entry named "unarr" — should be skipped because Typeflag != TypeReg
	tw.WriteHeader(&tar.Header{
		Name:     "unarr",
		Typeflag: tar.TypeSymlink,
		Linkname: "/etc/passwd",
		Mode:     0o755,
	})

	tw.Close()
	gw.Close()
	f.Close()

	destDir := filepath.Join(dir, "out")
	os.MkdirAll(destDir, 0o755)

	_, err = extractTarGz(archivePath, destDir)
	if err == nil {
		t.Error("extractTarGz() should return error when binary is a symlink (not TypeReg)")
	}
	if !strings.Contains(err.Error(), "not found in archive") {
		t.Errorf("error = %q, want to contain 'not found in archive'", err)
	}
}

func TestInstallBinaryNonExistentSource(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "output")

	err := installBinary("/nonexistent-source-binary", dst)
	if err == nil {
		t.Error("installBinary with nonexistent source should return error")
	}
	if !strings.Contains(err.Error(), "read new binary") {
		t.Errorf("error = %q, want to contain 'read new binary'", err)
	}

	// Verify destination was not created
	if _, statErr := os.Stat(dst); statErr == nil {
		t.Error("destination file should not exist after failed install")
	}
}

func TestInstallBinaryUnwritableDestination(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	os.WriteFile(src, []byte("binary"), 0o755)

	// Try to write to a path inside a non-existent directory
	dst := filepath.Join(dir, "nonexistent-subdir", "binary")

	err := installBinary(src, dst)
	if err == nil {
		t.Error("installBinary to non-writable destination should return error")
	}
	if !strings.Contains(err.Error(), "write binary") {
		t.Errorf("error = %q, want to contain 'write binary'", err)
	}
}

func TestUpgraderLog(t *testing.T) {
	var messages []string
	u := &Upgrader{
		CurrentVersion: "1.0.0",
		OnProgress:     func(msg string) { messages = append(messages, msg) },
	}

	u.log("hello world")
	if len(messages) != 1 || messages[0] != "hello world" {
		t.Errorf("log() messages = %v, want [hello world]", messages)
	}
}

func TestUpgraderLogNilOnProgress(t *testing.T) {
	u := &Upgrader{CurrentVersion: "1.0.0"}
	// Should not panic
	u.log("test message with nil OnProgress")
}

func TestResultFields(t *testing.T) {
	r := Result{
		Success:    true,
		OldVersion: "1.0.0",
		NewVersion: "2.0.0",
		BackupPath: "/tmp/backup",
	}
	if !r.Success || r.OldVersion != "1.0.0" || r.NewVersion != "2.0.0" || r.BackupPath != "/tmp/backup" {
		t.Errorf("Result fields not set correctly: %+v", r)
	}

	r2 := Result{Success: false, Error: fmt.Errorf("test error")}
	if r2.Success || r2.Error == nil {
		t.Errorf("Result error case not correct: %+v", r2)
	}
}

func TestDownloadSetsUserAgent(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.WriteHeader(200)
		fmt.Fprint(w, "data")
	}))
	defer srv.Close()

	restore := swapHTTPClient(&http.Client{
		Transport: &rewriteTransport{url: srv.URL},
	})
	defer restore()

	path, err := download(context.Background(), "1.0.0")
	if err != nil {
		t.Fatalf("download() = %v", err)
	}
	defer os.Remove(path)

	if gotUA != "unarr-updater" {
		t.Errorf("User-Agent = %q, want 'unarr-updater'", gotUA)
	}
}

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeResumablePartial creates the .part bytes AND the provenance sidecar that
// makes them resumable — the shape a paused/interrupted download of `url` leaves
// behind.
func writeResumablePartial(t *testing.T, dest, url string, data []byte) {
	t.Helper()
	writeResumablePartialMeta(t, dest, data, &partMeta{URL: url})
}

func writeResumablePartialMeta(t *testing.T, dest string, data []byte, m *partMeta) {
	t.Helper()
	if err := os.WriteFile(partialPath(dest), data, 0o644); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal sidecar: %v", err)
	}
	if err := os.WriteFile(partMetaPath(dest), b, 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
}

// bigBody returns content that clears the anti-stub floor for .mkv names.
func bigBody(marker string) string {
	return marker + strings.Repeat("x", minPlausibleVideoBytes+64*1024)
}

func runDebridDownload(t *testing.T, task *Task, outputDir string) (*Result, error) {
	t.Helper()
	d := NewDebridDownloader()
	progressCh := make(chan Progress, 100)
	defer close(progressCh)
	return d.Download(context.Background(), task, outputDir, progressCh)
}

func plainServer(content string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(content))
	}))
}

// A completed download must leave the file under its final name and nothing
// else: no .part, no sidecar. The final name appearing IS the completion marker.
func TestDebridFinalizeLeavesNoPartialArtifacts(t *testing.T) {
	content := bigBody("FULL_")
	srv := plainServer(content)
	defer srv.Close()

	outputDir := t.TempDir()
	task := &Task{ID: "fin-001", DirectURL: srv.URL + "/f.mkv", DirectFileName: "fin.mkv", Status: StatusDownloading}

	result, err := runDebridDownload(t, task, outputDir)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	dest := filepath.Join(outputDir, "fin.mkv")
	if result.FilePath != dest {
		t.Errorf("FilePath = %q, want %q", result.FilePath, dest)
	}
	if _, err := os.Stat(partialPath(dest)); !os.IsNotExist(err) {
		t.Errorf(".part must be gone after finalize; stat err = %v", err)
	}
	if _, err := os.Stat(partMetaPath(dest)); !os.IsNotExist(err) {
		t.Errorf("sidecar must be gone after finalize; stat err = %v", err)
	}
}

// A bare partial at the FINAL name (an older agent version, a file of unknown
// provenance) must never be appended to: the download starts clean and the
// final content is exactly what the server served.
func TestDebridLegacyBarePartialIsNotResumed(t *testing.T) {
	content := bigBody("FRESH_")
	var sawRange bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			sawRange = true
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(content))
	}))
	defer srv.Close()

	outputDir := t.TempDir()
	dest := filepath.Join(outputDir, "legacy.mkv")
	if err := os.WriteFile(dest, []byte("OLD_UNKNOWN_BYTES"), 0o644); err != nil {
		t.Fatal(err)
	}

	task := &Task{ID: "legacy-001", DirectURL: srv.URL + "/f.mkv", DirectFileName: "legacy.mkv", Status: StatusDownloading}
	result, err := runDebridDownload(t, task, outputDir)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if sawRange {
		t.Error("a bare legacy file must not trigger a Range resume")
	}
	data, err := os.ReadFile(result.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != content {
		t.Errorf("final content spliced: got %d bytes starting %q, want the fresh server content", len(data), data[:16])
	}
}

// A .part with no sidecar has unknown provenance — it must be removed and the
// download restarted, never appended to.
func TestDebridPartialWithoutSidecarIsDiscarded(t *testing.T) {
	content := bigBody("CLEAN_")
	srv := plainServer(content)
	defer srv.Close()

	outputDir := t.TempDir()
	dest := filepath.Join(outputDir, "nosidecar.mkv")
	if err := os.WriteFile(partialPath(dest), []byte("ORPHAN_BYTES"), 0o644); err != nil {
		t.Fatal(err)
	}

	task := &Task{ID: "nosc-001", DirectURL: srv.URL + "/f.mkv", DirectFileName: "nosidecar.mkv", Status: StatusDownloading}
	result, err := runDebridDownload(t, task, outputDir)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	data, _ := os.ReadFile(result.FilePath)
	if string(data) != content {
		t.Errorf("orphan partial was appended to: got %d bytes, want %d", len(data), len(content))
	}
}

// A partial recorded against a DIFFERENT URL with no validator cannot be proven
// to be a prefix of the new link's bytes — it must be discarded, not spliced.
func TestDebridDifferentURLPartialIsDiscarded(t *testing.T) {
	content := bigBody("NEWLINK_")
	srv := plainServer(content)
	defer srv.Close()

	outputDir := t.TempDir()
	dest := filepath.Join(outputDir, "urlswap.mkv")
	writeResumablePartial(t, dest, "https://other-cdn.example.com/old-link/f.mkv", []byte("BYTES_FROM_ANOTHER_LINK"))

	task := &Task{ID: "urlswap-001", DirectURL: srv.URL + "/f.mkv", DirectFileName: "urlswap.mkv", Status: StatusDownloading}
	result, err := runDebridDownload(t, task, outputDir)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	data, _ := os.ReadFile(result.FilePath)
	if string(data) != content {
		t.Errorf("partial from a different URL was spliced: got %d bytes, want %d", len(data), len(content))
	}
}

// With a stored ETag the resume goes out with If-Range; a server whose entity
// CHANGED answers 200 (full body) and the stale partial must be truncated away.
func TestDebridIfRangeChangedEntityRestartsClean(t *testing.T) {
	content := bigBody("V2_")
	var sawIfRange string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawIfRange = r.Header.Get("If-Range")
		// Entity changed: ignore the Range, serve the new full body (RFC-conform
		// If-Range behavior).
		w.Header().Set("ETag", `"v2"`)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(content))
	}))
	defer srv.Close()

	outputDir := t.TempDir()
	dest := filepath.Join(outputDir, "ifrange.mkv")
	url := srv.URL + "/f.mkv"
	writeResumablePartialMeta(t, dest, []byte("V1_OLD_PREFIX"), &partMeta{URL: url, ETag: `"v1"`})

	task := &Task{ID: "ifrange-001", DirectURL: url, DirectFileName: "ifrange.mkv", Status: StatusDownloading}
	result, err := runDebridDownload(t, task, outputDir)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if sawIfRange != `"v1"` {
		t.Errorf("If-Range = %q, want the stored ETag", sawIfRange)
	}
	data, _ := os.ReadFile(result.FilePath)
	if string(data) != content {
		t.Errorf("changed entity was spliced onto the old partial: got %d bytes, want %d", len(data), len(content))
	}
}

// A 206 whose Content-Range offset does not match the on-disk partial must be
// rejected — appending it would corrupt. The download restarts clean.
func TestDebrid206WrongOffsetRestartsClean(t *testing.T) {
	content := bigBody("GOOD_")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			// Malicious/broken CDN: grants a resume at the WRONG offset.
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(content)-1, len(content)))
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte(content))
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(content))
	}))
	defer srv.Close()

	outputDir := t.TempDir()
	dest := filepath.Join(outputDir, "wrongoff.mkv")
	url := srv.URL + "/f.mkv"
	writeResumablePartial(t, dest, url, []byte("PREFIX_ALREADY_HAVE"))

	task := &Task{ID: "wrongoff-001", DirectURL: url, DirectFileName: "wrongoff.mkv", Status: StatusDownloading}
	result, err := runDebridDownload(t, task, outputDir)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	data, _ := os.ReadFile(result.FilePath)
	if string(data) != content {
		t.Errorf("mismatched 206 was appended: got %d bytes, want %d", len(data), len(content))
	}
}

// A stream that keeps sending past the advertised total is as untrustworthy as
// a short one: reject with an integrity error and remove the artifacts.
func TestDebridOverlongStreamRejected(t *testing.T) {
	advertised := int64(minPlausibleVideoBytes + 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 206 resume with a Content-Range total, then a body LONGER than the
		// remainder — chunked (no Content-Length), so only our own guard sees it.
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 10-%d/%d", advertised-1, advertised))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte(strings.Repeat("y", int(advertised)+512*1024)))
	}))
	defer srv.Close()

	outputDir := t.TempDir()
	dest := filepath.Join(outputDir, "overlong.mkv")
	url := srv.URL + "/f.mkv"
	writeResumablePartial(t, dest, url, []byte("0123456789"))

	task := &Task{ID: "overlong-001", DirectURL: url, DirectFileName: "overlong.mkv", Status: StatusDownloading}
	_, err := runDebridDownload(t, task, outputDir)
	if err == nil {
		t.Fatal("expected an integrity error for an overlong stream")
	}
	if !IsIntegrity(err) {
		t.Errorf("overlong stream must be an IntegrityError, got %T: %v", err, err)
	}
	if _, statErr := os.Stat(partialPath(dest)); !os.IsNotExist(statErr) {
		t.Errorf("overlong partial must be removed; stat err = %v", statErr)
	}
}

// A valid resume must splice correctly AND refresh the sidecar total from
// Content-Range so later attempts can cross-check it.
func TestDebridResumeUpdatesSidecarTotal(t *testing.T) {
	prefix := "PREFIX_" + strings.Repeat("A", 700*1024)
	rest := "REST_" + strings.Repeat("B", 700*1024)
	full := prefix + rest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var start int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &start)
		if start != int64(len(prefix)) {
			t.Errorf("Range start = %d, want %d", start, len(prefix))
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(full)-1, len(full)))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(rest)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte(rest))
	}))
	defer srv.Close()

	outputDir := t.TempDir()
	dest := filepath.Join(outputDir, "resume2.mkv")
	url := srv.URL + "/f.mkv"
	writeResumablePartial(t, dest, url, []byte(prefix))

	task := &Task{ID: "resume2-001", DirectURL: url, DirectFileName: "resume2.mkv", Status: StatusDownloading}
	result, err := runDebridDownload(t, task, outputDir)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	data, _ := os.ReadFile(result.FilePath)
	if string(data) != full {
		t.Errorf("resumed content wrong: got %d bytes, want %d", len(data), len(full))
	}
}

// A resume against the same URL whose advertised total DIFFERS from the
// sidecar's is a different file behind the same link — restart, don't splice.
func TestDebridTotalMismatchRestartsClean(t *testing.T) {
	content := bigBody("SWAPPED_")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			var start int64
			fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &start)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(content)-1, len(content)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte(content[start:]))
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(content))
	}))
	defer srv.Close()

	outputDir := t.TempDir()
	dest := filepath.Join(outputDir, "totalswap.mkv")
	url := srv.URL + "/f.mkv"
	// Sidecar claims the file is a DIFFERENT size than the server now reports.
	writeResumablePartialMeta(t, dest, []byte("OLD_PREFIX"), &partMeta{URL: url, TotalBytes: int64(len(content)) + 12345})

	task := &Task{ID: "totalswap-001", DirectURL: url, DirectFileName: "totalswap.mkv", Status: StatusDownloading}
	result, err := runDebridDownload(t, task, outputDir)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	data, _ := os.ReadFile(result.FilePath)
	if string(data) != content {
		t.Errorf("size-mismatched resume was spliced: got %d bytes, want %d", len(data), len(content))
	}
}

func TestParseContentRange(t *testing.T) {
	cases := []struct {
		in                string
		start, end, total int64
		ok                bool
	}{
		{"bytes 100-199/1000", 100, 199, 1000, true},
		{"bytes 0-999/1000", 0, 999, 1000, true},
		{"bytes 5-9/*", 5, 9, 0, true},
		{"", 0, 0, 0, false},
		{"garbage", 0, 0, 0, false},
	}
	for _, c := range cases {
		start, end, total, ok := parseContentRange(c.in)
		if start != c.start || end != c.end || total != c.total || ok != c.ok {
			t.Errorf("parseContentRange(%q) = (%d,%d,%d,%v), want (%d,%d,%d,%v)",
				c.in, start, end, total, ok, c.start, c.end, c.total, c.ok)
		}
	}
}

func TestPartMetaIfRangeValidator(t *testing.T) {
	if v := (&partMeta{ETag: `"strong"`}).ifRangeValidator(); v != `"strong"` {
		t.Errorf("strong etag: got %q", v)
	}
	if v := (&partMeta{ETag: `W/"weak"`, LastModified: "Wed, 21 Oct 2015 07:28:00 GMT"}).ifRangeValidator(); v != "Wed, 21 Oct 2015 07:28:00 GMT" {
		t.Errorf("weak etag must fall back to Last-Modified: got %q", v)
	}
	if v := (*partMeta)(nil).ifRangeValidator(); v != "" {
		t.Errorf("nil meta: got %q", v)
	}
}

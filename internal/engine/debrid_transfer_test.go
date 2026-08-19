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
	"time"
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

// A link whose advertised Content-Length disagrees with the server-resolved
// file size is serving the WRONG file — refuse before downloading it.
func TestDebridExpectedSizeConflictRejected(t *testing.T) {
	content := bigBody("WRONGFILE_")
	srv := plainServer(content)
	defer srv.Close()

	outputDir := t.TempDir()
	task := &Task{
		ID:             "conflict-001",
		DirectURL:      srv.URL + "/f.mkv",
		DirectFileName: "conflict.mkv",
		DirectFileSize: int64(len(content)) + 999_999, // server resolved a DIFFERENT file
		Status:         StatusDownloading,
	}
	_, err := runDebridDownload(t, task, outputDir)
	if err == nil {
		t.Fatal("expected a size-conflict error")
	}
	if !IsIntegrity(err) {
		t.Errorf("size conflict must be an IntegrityError, got %T: %v", err, err)
	}
	dest := filepath.Join(outputDir, "conflict.mkv")
	if _, statErr := os.Stat(partialPath(dest)); !os.IsNotExist(statErr) {
		t.Errorf("conflicting partial must be removed; stat err = %v", statErr)
	}
}

// chunkedServer serves the body with no Content-Length (chunked), the shape of
// the CDN responses the expected size exists to police.
func chunkedServer(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
}

// With no Content-Length at all, the server-resolved size is what catches a
// truncated stream — before it, this exact shape sailed through as "complete".
func TestDebridExpectedSizeCatchesTruncationWithoutContentLength(t *testing.T) {
	full := bigBody("NOCL_")
	srv := chunkedServer(full[:len(full)-256*1024]) // short by 256 KiB
	defer srv.Close()

	task := &Task{
		ID:             "nocl-trunc-001",
		DirectURL:      srv.URL + "/f.mkv",
		DirectFileName: "nocl.mkv",
		DirectFileSize: int64(len(full)),
		Status:         StatusDownloading,
	}
	_, err := runDebridDownload(t, task, t.TempDir())
	if err == nil {
		t.Fatal("expected a truncation error: stream ended short of the resolved size")
	}
	if !IsIntegrity(err) {
		t.Errorf("want IntegrityError, got %T: %v", err, err)
	}
}

// The happy path of the same shape: exact expected bytes over chunked encoding
// must complete and finalize.
func TestDebridExpectedSizeExactChunkedCompletes(t *testing.T) {
	full := bigBody("NOCL_OK_")
	srv := chunkedServer(full)
	defer srv.Close()

	outputDir := t.TempDir()
	task := &Task{
		ID:             "nocl-ok-001",
		DirectURL:      srv.URL + "/f.mkv",
		DirectFileName: "noclok.mkv",
		DirectFileSize: int64(len(full)),
		Status:         StatusDownloading,
	}
	result, err := runDebridDownload(t, task, outputDir)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if result.Size != int64(len(full)) {
		t.Errorf("Size = %d, want %d", result.Size, len(full))
	}
}

// A tiny stub body must be deleted (never kept as a resumable partial) even
// when an expected size would otherwise classify it as merely "truncated" —
// stub bytes are not a prefix of the real file, resuming over them splices.
func TestDebridStubDeletedEvenWithExpectedSize(t *testing.T) {
	srv := chunkedServer(strings.Repeat("e", 12*1024)) // 12 KiB error-page stub
	defer srv.Close()

	outputDir := t.TempDir()
	task := &Task{
		ID:             "stub-exp-001",
		DirectURL:      srv.URL + "/f.mkv",
		DirectFileName: "stubexp.mkv",
		DirectFileSize: 4 << 30, // server resolved a 4 GiB file
		Status:         StatusDownloading,
	}
	_, err := runDebridDownload(t, task, outputDir)
	if err == nil {
		t.Fatal("expected a stub error")
	}
	dest := filepath.Join(outputDir, "stubexp.mkv")
	if _, statErr := os.Stat(partialPath(dest)); !os.IsNotExist(statErr) {
		t.Errorf("stub partial must be removed, not kept for resume; stat err = %v", statErr)
	}
	if _, statErr := os.Stat(partMetaPath(dest)); !os.IsNotExist(statErr) {
		t.Errorf("stub sidecar must be removed; stat err = %v", statErr)
	}
}

// A resumable partial recorded for a different total than the server now
// expects belongs to another file — it must be discarded, not appended to.
func TestDebridExpectedSizeDiscardsForeignPartial(t *testing.T) {
	content := bigBody("RIGHTFILE_")
	srv := plainServer(content)
	defer srv.Close()

	outputDir := t.TempDir()
	dest := filepath.Join(outputDir, "foreign.mkv")
	url := srv.URL + "/f.mkv"
	writeResumablePartialMeta(t, dest, []byte("OLD_FILE_PREFIX"), &partMeta{
		URL:        url,
		TotalBytes: int64(len(content)) + 4242, // recorded for a different file size
	})

	task := &Task{
		ID:             "foreign-001",
		DirectURL:      url,
		DirectFileName: "foreign.mkv",
		DirectFileSize: int64(len(content)),
		Status:         StatusDownloading,
	}
	result, err := runDebridDownload(t, task, outputDir)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	data, _ := os.ReadFile(result.FilePath)
	if string(data) != content {
		t.Errorf("foreign partial was spliced: got %d bytes, want %d", len(data), len(content))
	}
}

// REGRESSION (review): the server re-mints the debrid CDN link on every
// re-dispatch, and many CDNs send no ETag/Last-Modified. Resume must survive
// that — the partial is provably the same bytes when it belongs to the same
// (infohash, file), so a 40 GB download must not restart from zero.
func TestDebridResumesAcrossRemintedLinkSameTorrentFile(t *testing.T) {
	prefix := "PREFIX_" + strings.Repeat("A", 700*1024)
	rest := "REST_" + strings.Repeat("B", 700*1024)
	full := prefix + rest

	var sawRange bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var start int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &start); err == nil && start > 0 {
			sawRange = true
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(full)-1, len(full)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte(full[start:]))
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(full)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(full))
	}))
	defer srv.Close()

	outputDir := t.TempDir()
	dest := filepath.Join(outputDir, "remint.mkv")
	const hash = "abc123def456abc123def456abc123def456abc1"
	// Sidecar recorded against the PREVIOUS (now dead) link, no validators.
	writeResumablePartialMeta(t, dest, []byte(prefix), &partMeta{
		URL:      "https://cdn.example.com/OLD-EXPIRED-TOKEN/remint.mkv",
		InfoHash: hash,
		FileName: "remint.mkv",
	})

	task := &Task{
		ID:             "remint-001",
		InfoHash:       hash,
		DirectURL:      srv.URL + "/NEW-TOKEN/remint.mkv",
		DirectFileName: "remint.mkv",
		Status:         StatusDownloading,
	}
	result, err := runDebridDownload(t, task, outputDir)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if !sawRange {
		t.Error("a re-minted link for the SAME torrent file must resume, not restart from zero")
	}
	data, _ := os.ReadFile(result.FilePath)
	if string(data) != full {
		t.Errorf("resumed content wrong: got %d bytes, want %d", len(data), len(full))
	}
}

// A partial recorded for a DIFFERENT torrent must never be resumed just because
// the file name matches — the infohash is what proves the bytes.
func TestDebridDoesNotResumeAcrossDifferentInfoHash(t *testing.T) {
	content := bigBody("OTHER_")
	srv := plainServer(content)
	defer srv.Close()

	outputDir := t.TempDir()
	dest := filepath.Join(outputDir, "hashswap.mkv")
	writeResumablePartialMeta(t, dest, []byte("BYTES_OF_ANOTHER_TORRENT"), &partMeta{
		URL:      "https://cdn.example.com/old/hashswap.mkv",
		InfoHash: "1111111111111111111111111111111111111111",
		FileName: "hashswap.mkv",
	})

	task := &Task{
		ID:             "hashswap-001",
		InfoHash:       "2222222222222222222222222222222222222222",
		DirectURL:      srv.URL + "/f.mkv",
		DirectFileName: "hashswap.mkv",
		Status:         StatusDownloading,
	}
	result, err := runDebridDownload(t, task, outputDir)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	data, _ := os.ReadFile(result.FilePath)
	if string(data) != content {
		t.Errorf("partial of a different torrent was spliced: got %d bytes, want %d", len(data), len(content))
	}
}

// The finished Result must carry the server-resolved size so verify() — the
// cross-backend backstop — can enforce it independently of this transport.
func TestDebridResultCarriesExpectedBytes(t *testing.T) {
	content := bigBody("EXPECTED_")
	srv := plainServer(content)
	defer srv.Close()

	task := &Task{
		ID:             "expbytes-001",
		DirectURL:      srv.URL + "/f.mkv",
		DirectFileName: "expbytes.mkv",
		DirectFileSize: int64(len(content)),
		Status:         StatusDownloading,
	}
	result, err := runDebridDownload(t, task, t.TempDir())
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if result.ExpectedBytes != int64(len(content)) {
		t.Errorf("ExpectedBytes = %d, want %d", result.ExpectedBytes, len(content))
	}
	if err := verify(result); err != nil {
		t.Errorf("verify of a byte-exact download must pass: %v", err)
	}
}

// verify() must reject a file whose size disagrees with the server-resolved
// one even when the transport's own check didn't run (defense in depth).
func TestVerifyRejectsWrongSizeAgainstExpectedBytes(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "short.mkv")
	if err := os.WriteFile(p, []byte(strings.Repeat("z", minPlausibleVideoBytes+1024)), 0o644); err != nil {
		t.Fatal(err)
	}
	err := verify(&Result{FilePath: p, FileName: "short.mkv", ExpectedBytes: 8 << 30})
	if err == nil {
		t.Fatal("expected an integrity error for a size that disagrees with the resolved file")
	}
	if !IsIntegrity(err) {
		t.Errorf("want IntegrityError, got %T: %v", err, err)
	}
}

// A truncated stream of a KNOWN-size file keeps its partial for resume even
// when the bytes so far are below the anti-stub floor — they are a valid
// prefix, and deleting them would restart a huge download on every hiccup.
func TestDebridSmallTruncationKeepsPartialWhenExpectedKnown(t *testing.T) {
	full := bigBody("BIGFILE_")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(full)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(full[:8*1024])) // 8 KiB, well under the 1 MiB floor
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// CUT the connection mid-transfer (no clean EOF): the bytes so far are a
		// valid prefix, unlike a complete tiny error body.
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}
	}))
	defer srv.Close()

	outputDir := t.TempDir()
	task := &Task{
		ID:             "smalltrunc-001",
		DirectURL:      srv.URL + "/f.mkv",
		DirectFileName: "smalltrunc.mkv",
		DirectFileSize: int64(len(full)),
		Status:         StatusDownloading,
	}
	_, err := runDebridDownload(t, task, outputDir)
	if err == nil {
		t.Fatal("expected a truncation error")
	}
	dest := filepath.Join(outputDir, "smalltrunc.mkv")
	if _, statErr := os.Stat(partialPath(dest)); statErr != nil {
		t.Errorf("a valid prefix of a known-size file must be KEPT for resume: %v", statErr)
	}
}

// Cancel landing in the finalize window (file already renamed) must still
// delete it — cancel-and-delete means delete.
func TestDebridCancelRemovesFinalizedFile(t *testing.T) {
	outputDir := t.TempDir()
	dest := filepath.Join(outputDir, "finalized.mkv")
	if err := os.WriteFile(dest, []byte(bigBody("DONE_")), 0o644); err != nil {
		t.Fatal(err)
	}

	d := NewDebridDownloader()
	// Simulate the race window: the task is still tracked with its destPath
	// while the file has already been published under its final name.
	d.track("fin-race-001", dest, func() {}, nil)
	if err := d.Cancel("fin-race-001"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("cancel-and-delete must remove a just-finalized file; stat err = %v", err)
	}
}

// A queued task (blocked on the per-destination lock) must be cancellable —
// not parked until the other download finishes.
func TestPathLockerCtxAbortsWhileQueued(t *testing.T) {
	l := newPathLocker()
	release := l.Lock("/tmp/some/dest.mkv")
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := l.LockCtx(ctx, "/tmp/some/dest.mkv")
		done <- err
	}()
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Error("a cancelled waiter must not acquire the lock")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled waiter stayed parked behind the holder")
	}
}

// A validator alone must NOT authorize a resume: a CDN that ignores If-Range
// (or a proxy that strips it) answers 206 for a completely different file, and
// the offset/total checks would pass whenever the two happen to share a size.
// Provenance (same URL, or same torrent file) is what authorizes; the
// validator only strengthens it.
func TestDebridValidatorAloneDoesNotAuthorizeResume(t *testing.T) {
	content := bigBody("RIGHT_")
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
	dest := filepath.Join(outputDir, "validonly.mkv")
	// Different URL, different torrent — but it carries a strong ETag.
	writeResumablePartialMeta(t, dest, []byte("BYTES_OF_SOMETHING_ELSE"), &partMeta{
		URL:      "https://other-cdn.example.com/old/other.mkv",
		ETag:     `"strong-etag"`,
		InfoHash: "1111111111111111111111111111111111111111",
		FileName: "other.mkv",
	})

	task := &Task{
		ID:             "validonly-001",
		InfoHash:       "2222222222222222222222222222222222222222",
		DirectURL:      srv.URL + "/f.mkv",
		DirectFileName: "validonly.mkv",
		Status:         StatusDownloading,
	}
	result, err := runDebridDownload(t, task, outputDir)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if sawRange {
		t.Error("an unrelated partial must not be resumed just because it carries a validator")
	}
	data, _ := os.ReadFile(result.FilePath)
	if string(data) != content {
		t.Errorf("unrelated partial was spliced: got %d bytes, want %d", len(data), len(content))
	}
}

// REGRESSION (audit): two tasks can resolve to the SAME destination (a
// re-dispatch racing a retry, two releases with one filename). Cancel deletes
// by PATH, so cancelling one must never unlink the other's live partial or its
// just-finished file.
func TestDebridCancelDoesNotDeleteAnotherTasksFiles(t *testing.T) {
	outputDir := t.TempDir()
	dest := filepath.Join(outputDir, "shared.mkv")
	part := partialPath(dest)
	if err := os.WriteFile(part, []byte("SURVIVOR_BYTES"), 0o644); err != nil {
		t.Fatal(err)
	}

	d := NewDebridDownloader()
	// Task B is the live writer of this destination; task A is a duplicate that
	// gets cancelled.
	d.track("task-b-live", dest, func() {}, nil)
	d.track("task-a-dup", dest, func() {}, nil)

	if err := d.Cancel("task-a-dup"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	if _, err := os.Stat(part); err != nil {
		t.Errorf("cancelling a duplicate task must not unlink the live task's partial: %v", err)
	}
}

// …and once nobody else claims the destination, cancel-and-delete really deletes.
func TestDebridCancelDeletesWhenDestIsUnclaimed(t *testing.T) {
	outputDir := t.TempDir()
	dest := filepath.Join(outputDir, "solo.mkv")
	part := partialPath(dest)
	if err := os.WriteFile(part, []byte("DOOMED"), 0o644); err != nil {
		t.Fatal(err)
	}

	d := NewDebridDownloader()
	d.track("task-solo", dest, func() {}, nil)
	if err := d.Cancel("task-solo"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Errorf("cancel-and-delete must remove the partial; stat err = %v", err)
	}
}

// A partial whose sidecar carries a validator but belongs to ANOTHER torrent
// must not be resumed on the strength of that validator (audit finding #2).
func TestDebridCrossInfoHashWithValidatorIsNotResumed(t *testing.T) {
	content := bigBody("CORRECT_")
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
	dest := filepath.Join(outputDir, "xhash.mkv")
	writeResumablePartialMeta(t, dest, []byte("BYTES_OF_ANOTHER_TORRENT"), &partMeta{
		URL:          "https://other-cdn.example.com/old/xhash.mkv",
		LastModified: "Wed, 21 Oct 2015 07:28:00 GMT", // weak-ish validator
		InfoHash:     "1111111111111111111111111111111111111111",
		FileName:     "xhash.mkv",
	})

	task := &Task{
		ID:             "xhash-001",
		InfoHash:       "2222222222222222222222222222222222222222",
		DirectURL:      srv.URL + "/f.mkv",
		DirectFileName: "xhash.mkv",
		Status:         StatusDownloading,
	}
	result, err := runDebridDownload(t, task, outputDir)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if sawRange {
		t.Error("a partial from a DIFFERENT torrent must not be resumed, validator or not")
	}
	data, _ := os.ReadFile(result.FilePath)
	if string(data) != content {
		t.Errorf("cross-infohash partial was spliced: got %d bytes, want %d", len(data), len(content))
	}
}

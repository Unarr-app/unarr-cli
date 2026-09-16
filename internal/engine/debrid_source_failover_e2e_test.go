package engine

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// End-to-end at the CLI boundary: the first CDN dies after writing a partial,
// the web resolver returns another provider, and the second CDN must receive a
// Range request and complete the exact original bytes.
func TestDebridSourceFailoverResumesPartialAcrossProviders(t *testing.T) {
	payload := make([]byte, 256*1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	half := len(payload) / 2

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server cannot hijack connection")
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		writeTruncatedResponse(t, conn, rw, payload[:half], len(payload))
	}))
	defer first.Close()

	var sawRange atomic.Bool
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := fmt.Sprintf("bytes=%d-", half)
		if r.Header.Get("Range") != want {
			t.Errorf("Range = %q, want %q", r.Header.Get("Range"), want)
			http.Error(w, "range required", http.StatusBadRequest)
			return
		}
		sawRange.Store(true)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", half, len(payload)-1, len(payload)))
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)-half))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[half:])
	}))
	defer second.Close()

	dir := t.TempDir()
	reporter := makeProgressReporter()
	mgr := NewManager(ManagerConfig{
		MaxConcurrent: 1,
		OutputDir:     dir,
		ResolveSource: func(_ context.Context, taskID, sourceID string) (agent.Source, error) {
			if taskID != "source-failover-task" || sourceID != "debrid:premiumize" {
				return agent.Source{}, fmt.Errorf("unexpected resolve %s %s", taskID, sourceID)
			}
			return agent.Source{
				ID:         sourceID,
				ReleaseKey: "btih:" + "aabbcc",
				Relation:   "exact_release",
				Transport:  "debrid",
				Provider:   "premiumize",
				InfoHash:   "aabbcc",
				DirectURL:  second.URL,
				FileName:   "payload.bin",
				FileSize:   int64(len(payload)),
			}, nil
		},
	}, reporter, NewDebridDownloader())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go reporter.Run(ctx)
	mgr.Submit(ctx, agent.Task{
		ID:              "source-failover-task",
		InfoHash:        "aabbcc",
		Title:           "payload.bin",
		PreferredMethod: "debrid",
		DirectURL:       first.URL,
		DirectFileName:  "payload.bin",
		DirectFileSize:  int64(len(payload)),
		SourceSet: &agent.SourceSet{Version: 1, Sources: []agent.Source{
			{ID: "debrid:current", ReleaseKey: "btih:aabbcc", Relation: "exact_release", Transport: "debrid", InfoHash: "aabbcc", DirectURL: first.URL},
			{ID: "debrid:premiumize", ReleaseKey: "btih:aabbcc", Relation: "exact_release", Transport: "debrid", Provider: "premiumize", InfoHash: "aabbcc"},
		}},
	})
	mgr.Wait()

	if !sawRange.Load() {
		t.Fatal("second provider was not used for a ranged resume")
	}
	got, err := os.ReadFile(filepath.Join(dir, "payload.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatal("completed file differs from the original payload")
	}
}

// A clean EOF below the server-resolved size is an IntegrityError, not a
// transport error. It must still rotate to another provider and resume.
func TestDebridSourceFailoverRepairsCleanTruncation(t *testing.T) {
	payload := make([]byte, 256*1024)
	for i := range payload {
		payload[i] = byte(i % 241)
	}
	half := len(payload) / 2

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// No Content-Length: net/http uses a cleanly terminated chunked body, so
		// persistAndCheck classifies the short entity as "truncated".
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload[:half])
	}))
	defer first.Close()

	var sawRange atomic.Bool
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("Range"), fmt.Sprintf("bytes=%d-", half); got != want {
			t.Errorf("Range = %q, want %q", got, want)
			http.Error(w, "range required", http.StatusBadRequest)
			return
		}
		sawRange.Store(true)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", half, len(payload)-1, len(payload)))
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)-half))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[half:])
	}))
	defer second.Close()

	dir := runDebridRepairTask(t, first.URL, second.URL, "payload.bin", int64(len(payload)))
	if !sawRange.Load() {
		t.Fatal("second provider was not used after a clean truncation")
	}
	assertDownloadedPayload(t, dir, "payload.bin", payload)
}

// A tiny clean 200 for a video is usually an expired-link HTML/error body.
// The bad bytes are discarded, then SourceSet supplies a fresh provider.
func TestDebridSourceFailoverRepairsStubResponse(t *testing.T) {
	payload := make([]byte, 2*1024*1024)
	for i := range payload {
		payload[i] = byte(i % 239)
	}

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>expired link</html>"))
	}))
	defer first.Close()

	var secondCalls atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "" {
			t.Errorf("stub bytes must be discarded; unexpected Range %q", got)
		}
		secondCalls.Add(1)
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer second.Close()

	// Unknown initial size avoids turning the tiny response into a size-conflict
	// before the anti-stub guard can classify it.
	dir := runDebridRepairTask(t, first.URL, second.URL, "movie.mkv", 0)
	if secondCalls.Load() == 0 {
		t.Fatal("second provider was not used after a stub response")
	}
	assertDownloadedPayload(t, dir, "movie.mkv", payload)
}

// A CDN that streams past the provider-listed size is serving an invalid
// entity. Those bytes are discarded before trying the next provider.
func TestDebridSourceFailoverRepairsOverlongResponse(t *testing.T) {
	payload := make([]byte, 256*1024)
	for i := range payload {
		payload[i] = byte(i % 233)
	}
	overlong := append(append([]byte(nil), payload...), 0xff)

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush() // force chunked encoding; task size remains ground truth
		}
		_, _ = w.Write(overlong)
	}))
	defer first.Close()

	var secondCalls atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "" {
			t.Errorf("overlong bytes must be discarded; unexpected Range %q", got)
		}
		secondCalls.Add(1)
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer second.Close()

	dir := runDebridRepairTask(t, first.URL, second.URL, "payload.bin", int64(len(payload)))
	if secondCalls.Load() == 0 {
		t.Fatal("second provider was not used after an overlong response")
	}
	assertDownloadedPayload(t, dir, "payload.bin", payload)
}

func runDebridRepairTask(t *testing.T, firstURL, secondURL, fileName string, fileSize int64) string {
	t.Helper()
	dir := t.TempDir()
	reporter := makeProgressReporter()
	mgr := NewManager(ManagerConfig{
		MaxConcurrent: 1,
		OutputDir:     dir,
		ResolveSource: func(_ context.Context, taskID, sourceID string) (agent.Source, error) {
			if taskID != "integrity-failover-task" || sourceID != "debrid:premiumize" {
				return agent.Source{}, fmt.Errorf("unexpected resolve %s %s", taskID, sourceID)
			}
			return agent.Source{
				ID:         sourceID,
				ReleaseKey: "btih:aabbcc",
				Relation:   "exact_release",
				Transport:  "debrid",
				Provider:   "premiumize",
				InfoHash:   "aabbcc",
				DirectURL:  secondURL,
				FileName:   fileName,
				FileSize:   fileSize,
			}, nil
		},
	}, reporter, NewDebridDownloader())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	go reporter.Run(ctx)
	mgr.Submit(ctx, agent.Task{
		ID:              "integrity-failover-task",
		InfoHash:        "aabbcc",
		Title:           fileName,
		PreferredMethod: "debrid",
		DirectURL:       firstURL,
		DirectFileName:  fileName,
		DirectFileSize:  fileSize,
		SourceSet: &agent.SourceSet{Version: 1, Sources: []agent.Source{
			{ID: "debrid:current", ReleaseKey: "btih:aabbcc", Relation: "exact_release", Transport: "debrid", InfoHash: "aabbcc", DirectURL: firstURL},
			{ID: "debrid:premiumize", ReleaseKey: "btih:aabbcc", Relation: "exact_release", Transport: "debrid", Provider: "premiumize", InfoHash: "aabbcc"},
		}},
	})
	mgr.Wait()
	return dir
}

func assertDownloadedPayload(t *testing.T, dir, fileName string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatal("completed file differs from the original payload")
	}
}

func writeTruncatedResponse(t *testing.T, conn net.Conn, rw *bufio.ReadWriter, body []byte, total int) {
	t.Helper()
	defer conn.Close()
	if _, err := fmt.Fprintf(rw, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", total); err != nil {
		t.Error(err)
		return
	}
	if _, err := rw.Write(body); err != nil {
		t.Error(err)
		return
	}
	if err := rw.Flush(); err != nil {
		t.Error(err)
	}
}

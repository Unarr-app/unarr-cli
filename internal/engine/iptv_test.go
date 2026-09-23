package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// iptvServer serves one file with Range support. When stallFirst is set, the
// first full (non-Range) response writes `half` bytes and then stalls until the
// client goes away — a download the playback hold has to interrupt.
type iptvServer struct {
	body       string
	half       int
	stallFirst bool

	mu       sync.Mutex
	ranges   []string
	requests atomic.Int32
	stalled  chan struct{}
}

func (s *iptvServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := s.requests.Add(1)
	rng := r.Header.Get("Range")
	s.mu.Lock()
	s.ranges = append(s.ranges, rng)
	s.mu.Unlock()
	if rng != "" {
		var start int
		fmt.Sscanf(rng, "bytes=%d-", &start)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(s.body)-1, len(s.body)))
		w.Header().Set("Content-Length", fmt.Sprint(len(s.body)-start))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte(s.body[start:]))
		return
	}
	w.Header().Set("Content-Length", fmt.Sprint(len(s.body)))
	w.WriteHeader(http.StatusOK)
	if !s.stallFirst || n > 1 {
		_, _ = w.Write([]byte(s.body))
		return
	}
	_, _ = w.Write([]byte(s.body[:s.half]))
	w.(http.Flusher).Flush()
	close(s.stalled)
	<-r.Context().Done()
}

func (s *iptvServer) rangeHeaders() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ranges...)
}

// waitPartialSize waits until the client has written at least n bytes to the
// partial — the server flushing them is not the same as the client storing them.
func waitPartialSize(t *testing.T, path string, n int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fi, err := os.Stat(path); err == nil && fi.Size() >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("partial %s never reached %d bytes", path, n)
}

func iptvTask(id, url string) *Task {
	return &Task{
		ID:              id,
		PreferredMethod: string(MethodIPTV),
		DirectURL:       url,
		DirectFileName:  id + ".mkv",
		Status:          StatusDownloading,
	}
}

// iptvBody clears the 1 MiB anti-stub floor applied to .mkv results.
func iptvBody() string {
	return strings.Repeat("A", 700*1024) + strings.Repeat("B", 700*1024)
}

func TestIptvOnlyHandlesIptvTasksAndIgnoresTheLocalMethodOrder(t *testing.T) {
	d := NewIptvDownloader(NewPlaybackHold(time.Minute))
	ok, _ := d.Available(context.Background(), iptvTask("a", "http://x/1.mkv"))
	if !ok {
		t.Fatal("an IPTV task with a minted URL must be available")
	}
	notIptv := &Task{ID: "b", PreferredMethod: "auto", DirectURL: "http://x/1.mkv"}
	if ok, _ := d.Available(context.Background(), notIptv); ok {
		t.Fatal("a debrid direct URL must not be taken by the IPTV downloader")
	}
	if ok, _ := d.Available(context.Background(), iptvTask("c", "")); ok {
		t.Fatal("an IPTV task without its URL must not be available")
	}
	order := effectiveOrder(iptvTask("d", "u"), []string{"torrent", "debrid"})
	if len(order) != 1 || order[0] != MethodIPTV {
		t.Fatalf("effectiveOrder(iptv) = %v, want [iptv] regardless of preferred_methods", order)
	}
}

func TestIptvDownloadCompletesAsIptv(t *testing.T) {
	srv := &iptvServer{body: iptvBody()}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	d := NewIptvDownloader(NewPlaybackHold(time.Minute))
	res, err := d.Download(context.Background(), iptvTask("ok", ts.URL+"/movie/u/p/1.mkv"), t.TempDir(), make(chan Progress, 100))
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if res.Method != MethodIPTV {
		t.Fatalf("result method = %q, want iptv", res.Method)
	}
	if res.Size != int64(len(srv.body)) {
		t.Fatalf("size = %d, want %d", res.Size, len(srv.body))
	}
}

func TestIptvPlaybackPausesTheTransferAndResumesWithRange(t *testing.T) {
	srv := &iptvServer{body: iptvBody(), half: 700 * 1024, stallFirst: true, stalled: make(chan struct{})}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	hold := NewPlaybackHold(time.Minute)
	d := NewIptvDownloader(hold)
	type out struct {
		res *Result
		err error
	}
	done := make(chan out, 1)
	outputDir := t.TempDir()
	task := iptvTask("hold", ts.URL+"/movie/u/p/2.mkv")
	go func() {
		res, err := d.Download(context.Background(), task, outputDir, make(chan Progress, 100))
		done <- out{res, err}
	}()

	<-srv.stalled
	dest, _ := safePath(outputDir, debridFileName(task))
	waitPartialSize(t, partialPath(dest), int64(srv.half))
	hold.Set(true) // the user pressed Play on IPTV
	time.Sleep(150 * time.Millisecond)
	if n := srv.requests.Load(); n != 1 {
		t.Fatalf("requests while held = %d, want 1 (no reconnect during playback)", n)
	}
	select {
	case o := <-done:
		t.Fatalf("Download returned during playback: %v", o.err)
	default:
	}

	hold.Set(false) // playback ended
	var o out
	select {
	case o = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("download did not resume after playback ended")
	}
	if o.err != nil {
		t.Fatalf("Download: %v", o.err)
	}
	data, _ := os.ReadFile(o.res.FilePath)
	if string(data) != srv.body {
		t.Fatal("resumed file differs from the source")
	}
	ranges := srv.rangeHeaders()
	if len(ranges) != 2 || ranges[0] != "" || !strings.HasPrefix(ranges[1], "bytes=") || ranges[1] == "bytes=0-" {
		t.Fatalf("requests = %q, want a full GET then a Range resume past byte 0", ranges)
	}
}

func TestIptvDownloadsRunOneAtATime(t *testing.T) {
	first := &iptvServer{body: iptvBody(), half: 700 * 1024, stallFirst: true, stalled: make(chan struct{})}
	ts1 := httptest.NewServer(first)
	defer ts1.Close()
	second := &iptvServer{body: iptvBody()}
	ts2 := httptest.NewServer(second)
	defer ts2.Close()

	d := NewIptvDownloader(NewPlaybackHold(time.Minute))
	ctx1, cancel1 := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, err := d.Download(ctx1, iptvTask("one", ts1.URL+"/1.mkv"), t.TempDir(), make(chan Progress, 100))
		firstDone <- err
	}()
	<-first.stalled

	secondDone := make(chan error, 1)
	go func() {
		_, err := d.Download(context.Background(), iptvTask("two", ts2.URL+"/2.mkv"), t.TempDir(), make(chan Progress, 100))
		secondDone <- err
	}()
	time.Sleep(150 * time.Millisecond)
	if n := second.requests.Load(); n != 0 {
		t.Fatalf("second IPTV download opened %d connection(s) while the first was running", n)
	}

	cancel1() // the first ends (user cancelled it): the second gets its turn
	<-firstDone
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("second Download: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second IPTV download never got its turn")
	}
}

func TestIptvCancelDuringPlaybackRemovesThePartial(t *testing.T) {
	srv := &iptvServer{body: iptvBody(), half: 700 * 1024, stallFirst: true, stalled: make(chan struct{})}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	hold := NewPlaybackHold(time.Minute)
	d := NewIptvDownloader(hold)
	outputDir := t.TempDir()
	task := iptvTask("cancel", ts.URL+"/3.mkv")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := d.Download(ctx, task, outputDir, make(chan Progress, 100))
		done <- err
	}()
	<-srv.stalled
	hold.Set(true)
	dest, _ := safePath(outputDir, debridFileName(task))
	deadline := time.Now().Add(3 * time.Second)
	for !fileExists(partialPath(dest)) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // let the attempt unwind into the parked wait

	if err := d.Cancel(task.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	cancel()
	<-done
	if fileExists(partialPath(dest)) || fileExists(partMetaPath(dest)) {
		t.Fatal("cancel-and-delete during playback left the partial behind")
	}
}

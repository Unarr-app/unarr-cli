package srcproxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// Two readers asking for the same block while the link is still filling it (a
// retried segment, a seek-back): the one queued behind the link must get the
// block the holder just stored, not reopen the provider for the same bytes.
func TestSingleUpstreamQueuedReaderReusesTheBlockJustFetched(t *testing.T) {
	data := payload(3 * blockSize)
	var requests atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var first int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &first); err != nil {
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)-int(first)))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, len(data)-1, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		// Slow enough that the second reader queues on the link mid-block.
		const chunk = 16 << 10
		for off := first; off < int64(len(data)); off += chunk {
			end := min(off+chunk, int64(len(data)))
			if _, err := w.Write(data[off:end]); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	t.Cleanup(up.Close)
	p := startProxy(t, Options{
		URL: up.URL + "/x.mkv", PinHead: blockSize, PinTail: blockSize,
		CacheBytes: 16 * blockSize, SingleUpstream: true,
	})

	rng := fmt.Sprintf("bytes=0-%d", blockSize-1)
	done := make(chan error, 1)
	go func() {
		n, err := timedGet(p.ForegroundURL(), rng, 10*time.Second)
		if err == nil && n != blockSize {
			err = fmt.Errorf("first reader got %d bytes", n)
		}
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	n, err := timedGet(p.ForegroundURL(), rng, 10*time.Second)
	if err != nil || n != blockSize {
		t.Fatalf("second reader: n=%d err=%v", n, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("upstream requests = %d, want 1 (the queued reader re-fetched the block)", got)
	}
}

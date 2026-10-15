package metrics

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// barrier holds every caller until n of them are inside at once. If they never
// get there — because something serializes them — it gives up after a timeout
// and records it, so a regression fails instead of hanging.
type barrier struct {
	n        int
	mu       sync.Mutex
	in       int
	all      chan struct{}
	timedOut atomic.Bool
}

func newBarrier(n int) *barrier { return &barrier{n: n, all: make(chan struct{})} }

func (b *barrier) wait() {
	b.mu.Lock()
	b.in++
	if b.in == b.n {
		close(b.all)
	}
	b.mu.Unlock()
	select {
	case <-b.all:
	case <-time.After(5 * time.Second):
		b.timedOut.Store(true)
	}
}

// pollConcurrently runs n polls at once and waits for all of them.
func pollConcurrently(h *Hub, n int) {
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.Poll(at(0))
		}()
	}
	wg.Wait()
}

// The VPN reader (an IpcGet on the tunnel) and the download-state reader (a walk
// of the store) are slow: concurrent polls must not queue behind each other's.
func TestHubSlowReadersDoNotSerializePolls(t *testing.T) {
	const n = 4
	vpn, states := newBarrier(n), newBarrier(n)
	h := NewHub("v", nil)
	h.SetVPN(func() VPN { vpn.wait(); return VPN{} })
	h.SetDownloadStates(func() map[string]int { states.wait(); return map[string]int{} })

	pollConcurrently(h, n)
	if vpn.timedOut.Load() {
		t.Error("vpn() runs under the hub lock: concurrent polls serialized")
	}
	if states.timedOut.Load() {
		t.Error("states() runs under the hub lock: concurrent polls serialized")
	}
}

// onStall (a log line in the daemon) runs after the hub lock is released: a
// slow or re-entrant callback must not block every other poll.
func TestHubOnStallRunsOutsideLock(t *testing.T) {
	var h *Hub
	var blocked atomic.Bool
	fired := 0
	h = NewHub("v", func(StallEvent) {
		fired++
		done := make(chan struct{})
		go func() { h.Poll(at(40)); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			blocked.Store(true)
		}
	})
	h.AddSource(func() []Reading {
		return []Reading{{Session: Session{ID: "d", Kind: KindDownload, State: "downloading"}, RateKnown: true, ExpectFlow: true}}
	})
	for sec := 0; sec <= 35; sec += 5 {
		h.Poll(at(sec))
	}
	if fired != 1 {
		t.Fatalf("stall events = %d, want 1", fired)
	}
	if blocked.Load() {
		t.Fatal("onStall runs with the hub lock held: a poll from it blocked")
	}
}

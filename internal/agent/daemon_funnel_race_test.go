package agent

import (
	"sync"
	"testing"
)

// TestFunnelURLIsRaceFree: the funnel supervisor writes the URL from its own
// goroutine while the sync loop reads it for every heartbeat. Run under -race
// (make test) this is the whole point; without the mutex it fails there.
func TestFunnelURLIsRaceFree(t *testing.T) {
	SealState() // keep the state file off the disk for this test
	t.Cleanup(resetStateSeal)
	d := NewDaemon(DaemonConfig{}, nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				d.SetFunnelURL("https://a-b.trycloudflare.com")
				d.SetFunnelURL("")
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = d.FunnelURL()
			}
		}()
	}
	wg.Wait()
	if got := d.FunnelURL(); got != "" {
		t.Fatalf("last write was a clear, got %q", got)
	}
}

package logging

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf collects log output written from the Sweep goroutine.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// captureLog points the standard logger at a buffer for the test's duration.
func captureLog(t *testing.T) *syncBuf {
	t.Helper()
	buf := &syncBuf{}
	old := log.Default().Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(old) })
	return buf
}

// TestSweepReportsARotationFailureExactlyOnce. The 28 GB/day Windows pathology
// was invisible because the janitor did `_ = RotateNow(opts)` — every tick
// failed and nothing ever said a word. It must now say it once: repeating the
// complaint on every tick would flood the very log that cannot be rotated.
func TestSweepReportsARotationFailureExactlyOnce(t *testing.T) {
	path := unrotatableLog(t)
	buf := captureLog(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		Sweep(ctx, Options{Path: path, MaxSizeMB: 1, MaxFiles: 2}, 5*time.Millisecond)
	}()

	// One log line per report, whatever the message says inside it.
	const marker = "[logs] rotation of"
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), marker) {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("the janitor never reported a rotation it could not perform")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Many more ticks, all of them failing the same way.
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done

	if got := strings.Count(buf.String(), marker); got != 1 {
		t.Fatalf("the janitor logged the failure %d times over ~30 ticks, want 1:\n%s",
			got, buf.String())
	}
	if !strings.Contains(buf.String(), path) {
		t.Fatalf("the report does not name the log file:\n%s", buf.String())
	}
}

// TestSweepLeavesTheRingIntactWhenItCannotRotate: a janitor that shifted the
// ring on every failing tick would erase the real history in minutes while the
// live log never shrank. The write probe has to stop it before any of that.
func TestSweepLeavesTheRingIntactWhenItCannotRotate(t *testing.T) {
	path := unrotatableLog(t)
	if err := os.WriteFile(RotatedPath(path, 1), []byte("older run"), 0o644); err != nil {
		t.Fatalf("seed rotated slot: %v", err)
	}
	captureLog(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	Sweep(ctx, Options{Path: path, MaxSizeMB: 1, MaxFiles: 2}, 5*time.Millisecond)

	if got := mustRead(t, RotatedPath(path, 1)); got != "older run" {
		t.Fatalf("unarr.log.1 holds %d bytes of other content, want the %q it held",
			len(got), "older run")
	}
	if _, err := os.Stat(RotatedPath(path, 2)); !os.IsNotExist(err) {
		t.Fatalf("unarr.log.2 exists: the ring was shifted by a rotation that could not run")
	}
}

func TestSweepReturnsImmediatelyWhenRotationIsDisabled(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		Sweep(context.Background(), Options{Path: newTestLog(t), MaxSizeMB: 0}, time.Millisecond)
		Sweep(context.Background(), Options{Path: "", MaxSizeMB: 1}, time.Millisecond)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Sweep blocked on a configuration that disables rotation")
	}
}

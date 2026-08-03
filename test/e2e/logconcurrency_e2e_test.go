//go:build e2e

package e2e

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/logging"
)

// Concurrency shape: eight writers is more than the two (stdout + stderr) the
// daemon really points at one descriptor, and 2000 records each puts ~3.9 MiB
// through a 1 MiB budget — several rotations happen WHILE every goroutine is
// still writing, which is the only interesting moment.
const (
	concurrentWriters = 8
	recordsPerWriter  = 2000
	// keep is deliberately small so the ceiling actually bites: the assertion
	// worth making under load is that the ring stops growing, not that every
	// record is archived.
	concurrentKeep = 2
	// sweepInterval stands in for logging.DefaultSweepInterval, compressed so
	// the rotator turns many times inside a test that runs for a fraction of a
	// second.
	sweepInterval = time.Millisecond
)

// TestConcurrentWritesSurviveRotation runs the real production shape under
// load: many goroutines share ONE appending descriptor (the fd a daemon
// inherits from whoever launched it) while logging.RotateNow copy-truncates the
// same file from the outside, which is exactly what logging.Sweep does on its
// ticker. Nothing here rotates by closing or renaming — that path does not
// exist in production.
//
// What is asserted is what copy-truncate actually promises: the ring stays
// bounded, retention is respected, the holder never gets stranded on a dead
// inode, and no record is duplicated or shredded mid-file. Records lost in the
// window between the snapshot and the truncate are the documented cost of
// copytruncate, so they are counted and reported rather than failed on.
//
// Run with -race: the writers share the descriptor with the rotator.
func TestConcurrentWritesSurviveRotation(t *testing.T) {
	s := newSandbox(t)
	s.writeConfig(t, "[daemon]\nlog_max_size_mb = 1\nlog_max_files = 2\n")
	opts := ringOptions(t, s.cfgPath, s.logPath())
	if opts.MaxSizeMB != 1 || opts.MaxFiles != concurrentKeep {
		t.Fatalf("config did not reach the rotator: %+v", opts)
	}
	f := openDaemonLog(t, opts)

	stop := sweepInBackground(t, opts)
	var wg sync.WaitGroup
	errs := make(chan error, concurrentWriters)
	for g := 0; g < concurrentWriters; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < recordsPerWriter; i++ {
				// Globally unique sequence, so a lost or duplicated record is
				// identifiable rather than merely a count mismatch.
				if _, werr := f.Write([]byte(seedLine(g*recordsPerWriter + i))); werr != nil {
					errs <- werr
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for werr := range errs {
		t.Fatalf("concurrent write: %v", werr)
	}
	stop() // quiesce before inspecting the ring

	t.Logf("ring at %s:\n%s", s.logPath(), ringListing(s.logPath(), opts.MaxFiles))

	// The rotation storm has to have actually happened, or the rest proves
	// nothing about rotating under concurrent writers.
	if _, err := os.Stat(logging.RotatedPath(s.logPath(), 1)); err != nil {
		t.Fatalf("no rotation happened during the run: %v", err)
	}
	if _, err := os.Stat(logging.RotatedPath(s.logPath(), concurrentKeep+1)); !os.IsNotExist(err) {
		t.Errorf("unarr.log.%d exists with log_max_files = %d (stat err: %v)",
			concurrentKeep+1, concurrentKeep, err)
	}
	if got, ceiling := ringBytes(s.logPath(), opts.MaxFiles), int64((concurrentKeep+1)*mib+lineBytes); got > ceiling {
		t.Errorf("ring grew to %d bytes under load, ceiling is %d", got, ceiling)
	}

	assertRingIsIntactUnderLoad(t, s.logPath(), opts.MaxFiles)
	// The descriptor every goroutine shared must still reach the live file.
	assertHolderStillReachesTheLiveFile(t, f, s.logPath(), concurrentWriters*recordsPerWriter)
}

// sweepInBackground drives logging.RotateNow on a ticker for the duration of
// the run — logging.Sweep's body, with a stop hook so the test can quiesce the
// ring before reading it. Errors are swallowed exactly as Sweep swallows them.
func sweepInBackground(t *testing.T, opts logging.Options) func() {
	t.Helper()
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(sweepInterval)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				_ = logging.RotateNow(opts)
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done); wg.Wait() }) }
}

// assertRingIsIntactUnderLoad checks every slot holds whole records, that no
// record was written twice, and reports how many the copy-truncate race cost.
//
// A snapshot taken while writers append can only ever cut its own tail — the
// copy reads forward and appends land past the read offset — so a torn line is
// tolerated as the LAST line of a rotated slot and nowhere else. A tear in the
// middle would mean interleaved writes are being shredded, which is a real bug.
func assertRingIsIntactUnderLoad(t *testing.T, path string, keep int) {
	t.Helper()
	seen := make(map[int]bool)
	torn := 0
	for i := 0; i <= keep; i++ {
		whole, tail := slotRecords(t, ringSlot(path, i))
		if tail != "" {
			// Only a rotated snapshot may end mid-record; the live file was
			// quiesced before this ran.
			if i == 0 {
				t.Errorf("live log ends mid-record (%d bytes): %q", len(tail), truncateForLog(tail))
			}
			torn++
		}
		for _, ln := range whole {
			n := seqOf(ln)
			if n < 0 || len(ln)+1 != lineBytes {
				t.Fatalf("shredded record in the middle of slot %d: %q", i, truncateForLog(ln))
			}
			if seen[n] {
				t.Errorf("record %d appears twice in the ring", n)
			}
			seen[n] = true
		}
	}
	const written = concurrentWriters * recordsPerWriter
	if len(seen) == 0 {
		t.Fatal("ring is empty after the run")
	}
	if len(seen) > written {
		t.Errorf("ring holds %d records but only %d were written", len(seen), written)
	}
	t.Logf("%d of %d records from %d goroutines survived the rotation storm "+
		"(%d lost to the copy-truncate race, %d snapshot tails cut mid-record)",
		len(seen), written, concurrentWriters, written-len(seen), torn)
}

// slotRecords splits one ring slot into its complete records plus whatever
// trailing bytes did not end in a newline (the empty string when the file ends
// cleanly, or does not exist).
func slotRecords(t *testing.T, path string) ([]string, string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ""
		}
		t.Fatalf("read %s: %v", path, err)
	}
	parts := strings.Split(string(data), "\n")
	return parts[:len(parts)-1], parts[len(parts)-1]
}

// truncateForLog keeps a failure message readable when the offending record is
// a 256-byte line of padding.
func truncateForLog(s string) string {
	const max = 64
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

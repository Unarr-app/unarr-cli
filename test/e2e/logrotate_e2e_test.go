//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/logging"
)

// lineBytes is the fixed width of a seeded record, padding included. Fixed so a
// byte budget converts to an exact line count and a torn line is visible as a
// wrong length rather than as a subtle diff.
const lineBytes = 256

// mib is one mebibyte — the unit log_max_size_mb is expressed in.
const mib = 1024 * 1024

// seedLine renders record n at exactly lineBytes bytes, newline included.
func seedLine(n int) string {
	head := fmt.Sprintf("2026/08/03 10:00:00 [info] e2e seq=%08d ", n)
	return head + strings.Repeat("x", lineBytes-len(head)-1) + "\n"
}

// seqOf extracts the record number of a seeded line, or -1 when the line is not
// one whole seeded record (a torn write would land here).
func seqOf(line string) int {
	const marker = "seq="
	i := strings.Index(line, marker)
	if i < 0 || len(line) < i+len(marker)+8 {
		return -1
	}
	n, err := strconv.Atoi(line[i+len(marker) : i+len(marker)+8])
	if err != nil {
		return -1
	}
	return n
}

// ringOptions is what the CLI builds from the [daemon] log_* keys — resolved
// here through a real config file, so this exercises the same path
// logRingOptions() takes rather than hand-made Options.
func ringOptions(t *testing.T, cfgPath, logPath string) logging.Options {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return logging.Options{
		Path:      logPath,
		MaxSizeMB: cfg.Daemon.LogMaxSizeMB,
		MaxFiles:  cfg.Daemon.LogMaxFiles,
	}
}

// openDaemonLog returns the log descriptor the way production does: through
// logging.OpenFile, which is what daemon_control.go hands to the child process.
// The returned *os.File is the FOREIGN holder every rotation here happens
// underneath — nothing in this process rotates by writing to it.
func openDaemonLog(t *testing.T, opts logging.Options) *os.File {
	t.Helper()
	f, err := logging.OpenFile(opts)
	if err != nil {
		t.Fatalf("open daemon log: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// writeRecords appends n seeded records through the daemon's own descriptor,
// sweeping after each one. Rotation is therefore driven from OUTSIDE the writer
// exactly as logging.Sweep drives it in the daemon — the descriptor keeps
// pointing at the same file across every rotation.
func writeRecords(t *testing.T, f *os.File, opts logging.Options, from, n int) {
	t.Helper()
	for i := from; i < from+n; i++ {
		if _, err := f.Write([]byte(seedLine(i))); err != nil {
			t.Fatalf("write record %d: %v", i, err)
		}
		if err := logging.RotateNow(opts); err != nil {
			t.Fatalf("sweep after record %d: %v", i, err)
		}
	}
}

// TestRotationKeepsTheRingBoundedAndDropsTheOldestSlot writes several budgets'
// worth of records through the real production pair — an appending descriptor
// from logging.OpenFile plus logging.RotateNow copy-truncating underneath it —
// then checks on disk that the ring stopped growing and that the oldest records
// are gone rather than archived forever.
func TestRotationKeepsTheRingBoundedAndDropsTheOldestSlot(t *testing.T) {
	s := newSandbox(t)
	s.writeConfig(t, "[daemon]\nlog_max_size_mb = 1\nlog_max_files = 2\n")
	opts := ringOptions(t, s.cfgPath, s.logPath())
	if opts.MaxSizeMB != 1 || opts.MaxFiles != 2 {
		t.Fatalf("config did not reach the rotator: %+v", opts)
	}

	f := openDaemonLog(t, opts)
	// Five budgets' worth, so the ring has to drop slots repeatedly instead of
	// merely filling up once.
	total := 5 * mib / lineBytes
	writeRecords(t, f, opts, 0, total)

	t.Logf("ring at %s:\n%s", s.logPath(), ringListing(s.logPath(), opts.MaxFiles))

	for _, slot := range []int{0, 1, 2} {
		if _, err := os.Stat(ringSlot(s.logPath(), slot)); err != nil {
			t.Errorf("expected slot %d on disk: %v", slot, err)
		}
	}
	if _, err := os.Stat(logging.RotatedPath(s.logPath(), 3)); !os.IsNotExist(err) {
		t.Errorf("unarr.log.3 must never exist with log_max_files = 2 (stat err: %v)", err)
	}

	// The whole point: (keep+1) budgets is the ceiling, and one oversized record
	// is the only permitted overshoot.
	if got, ceiling := ringBytes(s.logPath(), opts.MaxFiles), int64(3*mib+lineBytes); got > ceiling {
		t.Errorf("ring grew to %d bytes, ceiling is %d", got, ceiling)
	}
	assertRingIsNewestSuffix(t, s.logPath(), opts.MaxFiles, total)

	// The descriptor opened before the first rotation must still reach the live
	// file: copy-truncate keeps the inode, a rename would have stranded it.
	assertHolderStillReachesTheLiveFile(t, f, s.logPath(), total)
}

// assertHolderStillReachesTheLiveFile writes one more record through the
// original descriptor and requires it to land in the live path. This is the
// failure copy-truncate exists to prevent: under a rename the holder keeps
// writing into the rotated inode and the live log stays frozen forever.
func assertHolderStillReachesTheLiveFile(t *testing.T, f *os.File, path string, seq int) {
	t.Helper()
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("live log missing after rotation: %v", err)
	}
	if _, err := f.Write([]byte(seedLine(seq))); err != nil {
		t.Fatalf("write through the original descriptor: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat live log: %v", err)
	}
	if after.Size() != before.Size()+lineBytes {
		t.Errorf("live log went %d → %d bytes, want a %d-byte record appended — "+
			"the holder is not writing into the live file",
			before.Size(), after.Size(), lineBytes)
	}
}

// assertRingIsNewestSuffix checks the surviving records are exactly the newest
// contiguous run: nothing torn, nothing duplicated, the oldest discarded.
//
// Exact because this test sweeps between writes, never during one — the
// copy-truncate race that does drop records is the concurrency suite's subject.
func assertRingIsNewestSuffix(t *testing.T, path string, keep, total int) {
	t.Helper()
	lines := ringLines(t, path, keep)
	if len(lines) == 0 {
		t.Fatal("ring is empty")
	}
	seqs := make([]int, 0, len(lines))
	for _, ln := range lines {
		n := seqOf(ln)
		if n < 0 || len(ln)+1 != lineBytes {
			t.Fatalf("torn record on disk: %q", ln)
		}
		seqs = append(seqs, n)
	}
	first, last := seqs[0], seqs[0]
	seen := make(map[int]bool, len(seqs))
	for _, n := range seqs {
		if seen[n] {
			t.Fatalf("record %d appears twice in the ring", n)
		}
		seen[n] = true
		if n < first {
			first = n
		}
		if n > last {
			last = n
		}
	}
	if last != total-1 {
		t.Errorf("newest record on disk is %d, wrote up to %d", last, total-1)
	}
	if first == 0 {
		t.Errorf("record 0 survived: nothing was discarded, the ring grows without bound")
	}
	if got := last - first + 1; got != len(seqs) {
		t.Errorf("ring holds %d records but spans %d..%d — a hole in the history", len(seqs), first, last)
	}
	t.Logf("ring holds records %d..%d (%d of %d written; %d oldest discarded)",
		first, last, len(seqs), total, first)
}

// TestRotationDisabledNeverRotates pins the documented escape hatch: with
// log_max_size_mb = 0 the file grows unbounded and no rotated sibling is ever
// created, because an external logrotate owns the file.
func TestRotationDisabledNeverRotates(t *testing.T) {
	s := newSandbox(t)
	s.writeConfig(t, "[daemon]\nlog_max_size_mb = 0\nlog_max_files = 3\n")
	opts := ringOptions(t, s.cfgPath, s.logPath())
	if opts.MaxSizeMB != 0 {
		t.Fatalf("log_max_size_mb = 0 did not reach the rotator: %+v", opts)
	}

	// writeRecords sweeps after every record; with rotation disabled every one
	// of those sweeps must be a no-op.
	f := openDaemonLog(t, opts)
	total := 3 * mib / lineBytes
	writeRecords(t, f, opts, 0, total)

	t.Logf("ring at %s:\n%s", s.logPath(), ringListing(s.logPath(), 3))

	fi, err := os.Stat(s.logPath())
	if err != nil {
		t.Fatalf("stat live log: %v", err)
	}
	if want := int64(total * lineBytes); fi.Size() != want {
		t.Errorf("live log is %d bytes, wrote %d — something rotated", fi.Size(), want)
	}
	for i := 1; i <= 4; i++ {
		if _, err := os.Stat(logging.RotatedPath(s.logPath(), i)); !os.IsNotExist(err) {
			t.Errorf("unarr.log.%d exists with rotation disabled (stat err: %v)", i, err)
		}
	}
	if got := len(ringLines(t, s.logPath(), 3)); got != total {
		t.Errorf("live log holds %d records, wrote %d", got, total)
	}
}

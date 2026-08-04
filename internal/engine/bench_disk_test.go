package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// smallBench shrinks the payload for the duration of a test. The assertions
// here are about cleanup and guard rails, not throughput, so writing the real
// 256 MiB would only cost CI time.
func smallBench(t *testing.T) {
	t.Helper()
	orig := diskBenchBytes
	diskBenchBytes = 1 << 20
	t.Cleanup(func() { diskBenchBytes = orig })
}

// The benchmark is allowed to be slow; it is never allowed to leave a
// multi-hundred-megabyte file in the user's download dir. Nothing else in the
// dir may be touched either.
func TestBenchmarkDiskWriteLeavesNothingBehind(t *testing.T) {
	smallBench(t)
	dir := t.TempDir()
	keep := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(keep, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := BenchmarkDiskWrite(context.Background(), dir)
	if err != nil {
		t.Fatalf("BenchmarkDiskWrite: %v", err)
	}
	if res.MBPerSec <= 0 {
		t.Errorf("write throughput = %v, want > 0", res.MBPerSec)
	}
	if res.Bytes != diskBenchBytes {
		t.Errorf("bytes = %d, want %d", res.Bytes, diskBenchBytes)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "movie.mkv" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir contents = %v, want only movie.mkv", names)
	}
}

// A cancelled context (Ctrl-C) must still take the temp file with it — that is
// the exit path a user is most likely to hit on a slow disk.
func TestBenchmarkDiskWriteCancelledStillCleansUp(t *testing.T) {
	smallBench(t)
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := BenchmarkDiskWrite(ctx, dir); err == nil {
		t.Fatal("expected an error from a cancelled benchmark")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("dir not empty after cancellation: %d entries", len(entries))
	}
}

// A file left by a previous SIGKILL is swept, so the dir cannot accumulate
// benchmark carcasses across runs.
func TestBenchmarkDiskWriteSweepsLeftovers(t *testing.T) {
	smallBench(t)
	dir := t.TempDir()
	stray := filepath.Join(dir, "unarr-bench-write-999.tmp")
	if err := os.WriteFile(stray, []byte("carcass"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := BenchmarkDiskWrite(context.Background(), dir); err != nil {
		t.Fatalf("BenchmarkDiskWrite: %v", err)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Errorf("leftover benchmark file survived: %v", err)
	}
}

func TestBenchmarkDiskWriteRejectsBadDir(t *testing.T) {
	if _, err := BenchmarkDiskWrite(context.Background(), ""); err == nil {
		t.Error("empty dir should error")
	}
	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := BenchmarkDiskWrite(context.Background(), missing); err == nil {
		t.Error("missing dir should error")
	}

	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := BenchmarkDiskWrite(context.Background(), file)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("err = %v, want 'not a directory'", err)
	}
}

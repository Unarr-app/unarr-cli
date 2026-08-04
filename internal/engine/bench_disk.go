package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// diskBenchBytes is how much we write. 256 MiB is a deliberate middle:
//
//   - Big enough that open/close/fsync latency is amortised into noise and a
//     spinning disk gets past its cache burst, so the number reflects sustained
//     throughput rather than a queue depth.
//   - Small enough to run in a few seconds even on a slow NAS (a 40 MB/s host
//     finishes in ~6 s) and to fit on the kind of small system disk a user might
//     have pointed download.dir at by accident.
//
// Note that SIZE is not what defeats the page cache — a host with 32 GB of RAM
// would happily absorb any size we picked. The fsync inside the timed section
// is what forces the bytes to the device; the size only makes the fsync's cost
// representative instead of dominant.
//
// A var, not a const, so tests can shrink it: what they assert is the cleanup
// and the guard rails, and burning 256 MiB of real I/O per case to check that a
// temp file was removed would only make the suite slower, never stricter.
var diskBenchBytes int64 = 256 << 20

// diskBenchChunk is the write unit. 4 MiB keeps the syscall count low without
// making cancellation coarse: the ctx is checked between chunks, so this also
// bounds how long a Ctrl-C waits before the temp file is removed.
const diskBenchChunk = 4 << 20

// diskBenchPattern is the temp-file name. It carries the tool name, the word
// "bench" and a .tmp suffix so that a user who finds one in their download dir
// after a hard kill knows on sight what it is and that it is disposable. It is
// also what sweepDiskBenchLeftovers globs — nothing else may match it, since
// anything that does gets deleted. (`unarr clean` deliberately never touches
// the download dir, so the sweep is the only reaper.)
const diskBenchPattern = "unarr-bench-write-*.tmp"

// diskBenchHeadroomFactor is how much free space we insist on, as a multiple of
// the write itself. A benchmark that fills the download dir is a bug that costs
// the user a download, so refuse rather than squeeze into the last free
// megabytes.
const diskBenchHeadroomFactor = 3

// DiskBenchResult is a sequential-write measurement of one directory.
// MBPerSec is decimal MB (10^6), the unit drive vendors and every other
// throughput readout in this CLI use.
type DiskBenchResult struct {
	Dir       string  `json:"dir"`
	Bytes     int64   `json:"bytes"`
	Seconds   float64 `json:"seconds"`
	MBPerSec  float64 `json:"writeMBps"`
	Synced    bool    `json:"fsync"`
	FreeBytes int64   `json:"freeBytes,omitempty"`
}

// BenchmarkDiskWrite measures sequential write throughput INSIDE dir — the
// download dir — because that is the filesystem downloads and HLS segments
// actually land on, and it is routinely not the one the binary runs from (USB
// enclosure, SMB share, spinning array). Measuring anywhere else would answer a
// question nobody asked.
//
// The temp file never escapes dir and is removed on every exit path, including
// a cancelled ctx (Ctrl-C): the removal is deferred before the first byte is
// written. Only SIGKILL can leave one behind, which is why the run starts by
// sweeping strays from a previous kill.
func BenchmarkDiskWrite(ctx context.Context, dir string) (DiskBenchResult, error) {
	res := DiskBenchResult{Dir: dir, Bytes: diskBenchBytes, Synced: true}
	if dir == "" {
		return res, errors.New("no download directory configured")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return res, fmt.Errorf("download dir %s: %w", dir, err)
	}
	if !info.IsDir() {
		return res, fmt.Errorf("download dir %s is not a directory", dir)
	}
	if err := checkDiskBenchSpace(dir, &res); err != nil {
		return res, err
	}
	sweepDiskBenchLeftovers(dir)

	f, err := os.CreateTemp(dir, diskBenchPattern)
	if err != nil {
		return res, fmt.Errorf("create benchmark file in %s: %w", dir, err)
	}
	path := f.Name()
	// Ordered so the file is closed before it is removed (Windows refuses to
	// unlink an open file), and so BOTH run on a cancelled ctx.
	defer os.Remove(path)
	defer func() { _ = f.Close() }()

	elapsed, err := writeDiskBenchPayload(ctx, f)
	if err != nil {
		return res, err
	}
	res.Seconds = elapsed.Seconds()
	if res.Seconds > 0 {
		res.MBPerSec = float64(diskBenchBytes) / 1e6 / res.Seconds
	}
	return res, nil
}

// checkDiskBenchSpace refuses to run when the write would eat the user's
// headroom. A failed statfs (unreadable network share) is NOT fatal: the write
// itself will fail loudly enough, and blocking the benchmark on a probe that
// cannot answer would be worse than attempting it.
func checkDiskBenchSpace(dir string, res *DiskBenchResult) error {
	free, _, err := agent.DiskInfo(dir)
	if err != nil {
		return nil
	}
	res.FreeBytes = free
	if free < diskBenchBytes*diskBenchHeadroomFactor {
		return fmt.Errorf("not enough free space in %s: %d MB free, benchmark needs %d MB plus headroom",
			dir, free/(1<<20), diskBenchBytes/(1<<20))
	}
	return nil
}

// sweepDiskBenchLeftovers removes benchmark files a previous run could not
// clean up (SIGKILL, power loss). Best-effort: a file we cannot delete is
// someone else's problem, not a reason to abort the measurement.
func sweepDiskBenchLeftovers(dir string) {
	matches, err := filepath.Glob(filepath.Join(dir, diskBenchPattern))
	if err != nil {
		return
	}
	for _, m := range matches {
		os.Remove(m)
	}
}

// writeDiskBenchPayload writes diskBenchBytes and fsyncs, returning the wall
// time of both. The fsync is INSIDE the timed section on purpose: without it we
// would be timing memcpy into the page cache and reporting RAM speed as disk
// speed.
func writeDiskBenchPayload(ctx context.Context, f *os.File) (time.Duration, error) {
	buf := incompressibleChunk(diskBenchChunk)
	start := time.Now()
	for written := int64(0); written < diskBenchBytes; {
		if err := ctx.Err(); err != nil {
			return 0, err // Ctrl-C — the deferred remove takes the file with it
		}
		n := len(buf)
		if remaining := diskBenchBytes - written; remaining < int64(n) {
			n = int(remaining)
		}
		if _, err := f.Write(buf[:n]); err != nil {
			return 0, fmt.Errorf("benchmark write: %w", err)
		}
		written += int64(n)
	}
	if err := f.Sync(); err != nil {
		return 0, fmt.Errorf("benchmark fsync: %w", err)
	}
	return time.Since(start), nil
}

// incompressibleChunk builds a non-repeating buffer. A block of zeroes would be
// swallowed by transparent compression (ZFS/Btrfs) or sparse-file handling and
// report a throughput the disk cannot deliver for real media. Same trick, same
// reason, as the /speedtest payload.
func incompressibleChunk(size int) []byte {
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = byte((i*31 + 7) & 0xff)
	}
	return buf
}

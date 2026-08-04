package logging

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

const (
	// DefaultMaxFiles is how many rotated siblings are kept (unarr.log.1 … .3),
	// so the whole ring costs at most (DefaultMaxFiles+1) size budgets. This is
	// the fallback for an unset Options.MaxFiles, and the single definition of
	// the number — internal/config derives its own default from it.
	DefaultMaxFiles = 3

	logFileMode = 0o644
	logDirMode  = 0o755
	bytesPerMB  = 1024 * 1024
)

// Options configures rotation and every rotation-aware reader.
type Options struct {
	// Path is the live log file. Rotated copies are Path+".1", Path+".2", …
	Path string
	// MaxSizeMB is the size budget for Path. 0 disables rotation entirely, and
	// that is what every caller in this repo resolves to by default: rotation is
	// opt-in behind [daemon] log_max_size_mb, so Writer, RotateNow and Sweep are
	// all no-ops until a user sets it. See docs/plans/daemon-log-ownership.md
	// ("Deuda abierta") for what turning it on still costs.
	MaxSizeMB int
	// MaxFiles is how many rotated siblings to keep. <=0 means DefaultMaxFiles;
	// "keep nothing" is spelled MaxSizeMB=0 plus your own cleanup, because a
	// zero here is far more likely to be an unset struct field than an intent.
	MaxFiles int
	// Owner answers whether a live process owns Path and rotates it itself. Set
	// it on every EXTERNAL rotation (RotateNow, Sweep, the installers' pre-launch
	// trim); leave it nil when this process is the owner — a Writer must never
	// refuse to rotate its own file. See OwnerProbe.
	Owner OwnerProbe
}

// maxBytes is the size budget in bytes, or 0 when rotation is off.
func (o Options) maxBytes() int64 {
	if o.MaxSizeMB <= 0 {
		return 0
	}
	return int64(o.MaxSizeMB) * bytesPerMB
}

// keep is the number of rotated siblings to retain.
func (o Options) keep() int {
	if o.MaxFiles <= 0 {
		return DefaultMaxFiles
	}
	return o.MaxFiles
}

// OpenFile rotates path when it is already over budget and returns an
// appending *os.File.
//
// Why a raw *os.File and not a self-rotating io.Writer: os/exec hands an
// *os.File to the child as a real descriptor, while ANY other io.Writer makes
// it spawn a copier goroutine in the PARENT — and the process that launches a
// detached daemon exits seconds later, which would leave the daemon writing
// into a broken pipe. So the child gets the descriptor and the parent rotates
// in the gap before the handover; live rotation for that daemon is then
// RotateNow's job, driven by Sweep.
func OpenFile(opts Options) (*os.File, error) {
	if opts.Path == "" {
		return nil, errors.New("logging: no log path")
	}
	if err := os.MkdirAll(filepath.Dir(opts.Path), logDirMode); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	// Best-effort: a log that could not be rotated is still a log worth writing
	// to, and refusing to start a daemon over it would be absurd.
	_ = RotateNow(opts)
	f, err := os.OpenFile(opts.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, logFileMode)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	return f, nil
}

// RotateNow rotates path if it already sits at or over the budget, using
// COPY-TRUNCATE instead of a rename.
//
// This is the rotation for a file this process does NOT own, and that set is
// permanent: unarr.boot.log while launchd or a detached parent holds it, the
// legacy unarr.err.log, and unarr.log itself on an install whose supervisor
// still owns the redirect, on `unarr logs rotate` against a stopped daemon, and
// in the installers' pre-launch trim. Renaming under a foreign holder does not
// shrink anything — the holder keeps appending to the renamed inode and the
// "fresh" log stays empty forever. Copying the contents aside and truncating in
// place is what is left.
//
// It only works where the holder both keeps append semantics AND leaves the
// file writable, and that is NOT uniform across platforms:
//
//   - POSIX (launchd, the detached launcher) opens with O_APPEND. The kernel
//     recomputes the offset from the file length on every write, so the next
//     line lands at 0 and the file really does shrink. Pinned by
//     TestRotateNowUnderAPosixAppendHolder.
//   - Windows, MEASURED on the VM harness: cmd.exe's `>>` redirect grants only
//     FILE_SHARE_READ, and os.Truncate is OpenFile(name, O_WRONLY, 0666) plus
//     Ftruncate — its GENERIC_WRITE is a sharing violation, so the truncate
//     fails outright ("The process cannot access the file because it is being
//     used by another process"). Copy-truncate can therefore never bound
//     anything cmd.exe holds; that path now has the daemon own its log and
//     rotate it by rename through Writer.
//
// Two guards run BEFORE any of it, in this order:
//
//  1. Options.Owner — EXPLICIT ownership. A log a live daemon owns is rotated
//     by that daemon, from the inside; copy-truncating it from here loses the
//     lines between the copy and the truncate and leaves the owner's in-memory
//     size counter pointing at a file that no longer has those bytes. This is
//     the guard, and it is the only one that can see a Go owner.
//  2. probeTruncatable — a cheap fail-fast, NOT a safety mechanism. It exists
//     so a holder that denies write access is rejected before a whole-file copy
//     is made, not after: on Windows that copy ran on every 60s sweep and cost
//     ~28 GB/day while the live log never shrank. Correctness does not depend
//     on it — rotateThroughStaging keeps the ring intact when the truncate
//     fails anyway.
//
// The remaining cost is a small race: lines written between the copy and the
// truncate are lost. That is the same trade logrotate's copytruncate makes, and
// losing a handful of log lines once per full log beats losing the disk.
//
// A no-op when rotation is disabled (MaxSizeMB 0) or the file is still small.
func RotateNow(opts Options) error {
	max := opts.maxBytes()
	if opts.Path == "" || max == 0 {
		return nil
	}
	fi, err := os.Stat(opts.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat log file: %w", err)
	}
	if fi.Size() < max {
		return nil
	}
	if err := opts.refuseIfOwned(); err != nil {
		return err
	}
	if err := probeTruncatable(opts.Path); err != nil {
		return cannotRotateInPlace(opts.Path, err)
	}
	if err := rotateThroughStaging(opts.Path, opts.keep(), copyThenTruncate); err != nil {
		return cannotRotateInPlace(opts.Path, err)
	}
	return nil
}

// cannotRotateInPlace is the one message an outsider's failed rotation returns.
// `unarr logs rotate` prints it, so it has to say what to DO, not just what
// failed.
func cannotRotateInPlace(path string, err error) error {
	return fmt.Errorf("cannot rotate %s in place: %w; another process holds it "+
		"without granting write access. A running daemon owns and rotates its own "+
		"log — rotate it from there, or stop whatever else is holding the file",
		path, err)
}

// probeTruncatable reports whether os.Truncate would be allowed to run, without
// touching a byte.
//
// On Windows os.Truncate is literally OpenFile(name, O_WRONLY, 0666) followed by
// Ftruncate, so this is THE SAME open with the same sharing semantics: it fails
// exactly when a holder such as cmd.exe's `>>` granted only FILE_SHARE_READ. On
// POSIX it is a slightly stricter check than syscall.Truncate needs — the same
// write permission, obtained via an open — costing one open/close and never
// rejecting a case truncate would have accepted.
//
// WHAT IT CANNOT DO, ever: detect a Go owner. Go asks for
// FILE_SHARE_READ|FILE_SHARE_WRITE on Windows and takes no lock on POSIX, so a
// daemon writing the file through its own descriptor lets this open succeed.
// Ownership is Options.Owner's job; this is only a cost saver.
//
// No O_TRUNC, no O_APPEND, no write.
func probeTruncatable(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	return f.Close()
}

// RotatedPath returns the n-th rotated sibling of a log path (n starts at 1).
func RotatedPath(path string, n int) string { return path + "." + strconv.Itoa(n) }

// RotatedPaths lists every rotated slot for a log path, newest (".1") first.
// `unarr clean` sweeps the whole ring with it, and the reader walks back
// through it when the live file holds fewer lines than asked for.
func RotatedPaths(path string, keep int) []string {
	if keep <= 0 {
		keep = DefaultMaxFiles
	}
	out := make([]string, 0, keep)
	for i := 1; i <= keep; i++ {
		out = append(out, RotatedPath(path, i))
	}
	return out
}

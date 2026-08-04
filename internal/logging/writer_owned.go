package logging

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Writer is a size-bounded log file that THIS PROCESS OWNS: it holds the only
// descriptor on the path and rotates by renaming the live file aside.
//
// Ownership is the whole contract, and it is what makes this different from
// RotateNow's copy-truncate:
//
//   - The descriptor is opened O_APPEND, which Go maps to a real
//     FILE_APPEND_DATA handle on Windows, so every write lands at the end of
//     whatever file the handle currently points at.
//   - Rotation closes that handle, renames the file aside (through
//     rotateThroughStaging, so a refused rename costs the ring nothing) and
//     reopens the path. A rename only shrinks the live file for the process
//     holding the descriptor — which is why this type is unusable for a log some
//     supervisor holds (launchd's StandardOutPath, cmd.exe's `>>` redirect,
//     the fd a detached child inherited). Those keep using RotateNow.
//   - The size counter lives in memory, seeded from Stat at open. It is exact
//     only because nothing else appends. If something else does, the budget is
//     under-counted and the file runs a little over — never unbounded.
//
// The mutex covers the whole of Write so a write and a rotation can never
// interleave: log.Logger serializes its own callers, but a second logger or a
// JSON encoder pointed at the same Writer would not.
//
// Not in scope, deliberately: time-based rotation, compression, SIGHUP reopen.
// MaxSizeMB = 0 disables rotation entirely and the Writer degrades to a plain
// appending file. That is the DEFAULT here — rotation is opt-in — so this is
// the mode the daemon actually runs in unless the user set log_max_size_mb.
type Writer struct {
	mu       sync.Mutex
	path     string
	max      int64 // budget in bytes; 0 = rotation disabled
	keep     int
	f        *os.File
	size     int64 // bytes in the live file, tracked in memory
	rotateAt int64 // rotate when size >= rotateAt; 0 = never
	warned   bool  // a rotation failure was already reported once
}

// NewWriter opens (creating it, and its directory, if needed) the log file this
// process is about to own.
//
// It does NOT rotate up front even when the file already sits over budget: the
// first Write does that, so there is one rotation path instead of two. The size
// counter is seeded from the file on disk rather than from zero — otherwise a
// restart would hand the file a fresh budget, and a 20 MB cap would quietly
// become 20 MB per restart.
func NewWriter(opts Options) (*Writer, error) {
	if opts.Path == "" {
		return nil, errors.New("logging: no log path")
	}
	if err := os.MkdirAll(filepath.Dir(opts.Path), logDirMode); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	w := &Writer{path: opts.Path, max: opts.maxBytes(), keep: opts.keep()}
	w.rotateAt = w.max
	if err := w.openLocked(); err != nil {
		return nil, err
	}
	return w, nil
}

// Path is the live log file this Writer owns.
func (w *Writer) Path() string { return w.path }

// Write appends p and rotates once the file reaches its budget.
//
// It always returns the WRITE's result, never the rotation's: a failed rotation
// must not make the caller believe the line was lost. Rotation trouble is
// reported out of band, once, on stderr.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		// A previous reopen failed; this is the retry.
		if err := w.openLocked(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	if err == nil && w.rotateAt > 0 && w.size >= w.rotateAt {
		w.rotateIfStillOverLocked()
	}
	return n, err
}

// rotateIfStillOverLocked re-reads the real file length before rotating, and
// rotates only if the file is still over budget. Caller holds mu.
//
// The counter is in memory and exact only while nothing else touches the file
// — but something else CAN: an external copy-truncate (an installer, an older
// build's janitor, `unarr self-update` trimming the log before restarting the
// daemon), or an operator's `> unarr.log`. Without this reconciliation the next
// line would rotate on a stale counter and shift the ring for a file that is
// already empty: one external trim would cost TWO ring shifts and produce one
// empty slot. One Stat per rotation, not per write.
func (w *Writer) rotateIfStillOverLocked() {
	if fi, err := w.f.Stat(); err == nil && fi.Size() < w.size {
		w.size = fi.Size()
		if w.size < w.rotateAt {
			return
		}
	}
	w.rotateLocked()
}

// Close releases the descriptor. Idempotent.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// openLocked opens the live file and re-seeds the size counter from disk. On
// failure it leaves the Writer descriptor-less so the next Write retries.
// Caller holds mu.
func (w *Writer) openLocked() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, logFileMode)
	if err != nil {
		w.f, w.size = nil, 0
		return fmt.Errorf("open log file: %w", err)
	}
	w.f, w.size = f, 0
	if fi, serr := f.Stat(); serr == nil {
		w.size = fi.Size()
	}
	return nil
}

// rotateLocked moves the live file into slot 1 and reopens the path. Caller
// holds mu. It reports nothing to the caller by design — see Write.
//
// It goes through rotateThroughStaging like every other rotation, so a rename
// the OS refuses leaves the ring exactly as it was. That is not theoretical: on
// Windows a plain `unarr logs -f` (the command `daemon install` itself prints)
// holds the live file without FILE_SHARE_DELETE, MoveFileEx fails, and the
// previous order — shift first, rename second — emptied one history slot per
// budget until all three were gone while the live log grew without a ceiling.
// Nothing could break the loop: the follower only releases its handle when it
// notices a rotation, which is the very thing its handle prevents.
func (w *Writer) rotateLocked() {
	// MANDATORY before the rename: Go opens files without FILE_SHARE_DELETE, so
	// Windows refuses to rename a file this process still has open.
	_ = w.f.Close()
	w.f = nil

	before := w.size
	renameErr := rotateThroughStaging(w.path, w.keep, renameLive)
	// Reopen whether or not the rename worked: a daemon that stops logging is
	// worse than one that logs too much.
	openErr := w.openLocked()

	if renameErr == nil {
		w.rotateAt = w.max
	} else {
		// BACK OFF BY A FULL BUDGET. Without this a permanently blocked rename
		// (a Windows reader holding the file, a CIFS share) costs one Rename
		// syscall PER LOG LINE — a different silent pathology from the one this
		// design exists to fix.
		base := w.size
		if openErr != nil {
			base = before // could not measure; assume the file did not shrink
		}
		w.rotateAt = base + w.max
	}
	if renameErr != nil {
		w.reportOnce(fmt.Errorf("rotate log file: %w", renameErr))
		return
	}
	if openErr != nil {
		w.reportOnce(openErr)
	}
}

// reportOnce writes a single line to stderr the first time rotation fails. The
// Writer cannot report through itself, and under this design stderr is the
// supervisor-held boot log — the right destination for "the log writer broke".
// Caller holds mu.
func (w *Writer) reportOnce(err error) {
	if w.warned {
		return
	}
	w.warned = true
	fmt.Fprintf(os.Stderr, "unarr: %v — logging continues, but %s may grow past its budget\n", err, w.path)
}

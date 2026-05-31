package engine

import (
	"errors"
	"fmt"
	"log"

	"github.com/torrentclaw/unarr/internal/agent"
)

// InsufficientDiskError is returned by CheckDiskSpace when a download's expected
// size (plus a reserve that keeps the filesystem healthy) won't fit in the free
// space of its target directory. The manager treats it as terminal — it does NOT
// fall back to another source (a different source would fill the same disk) and
// surfaces the message to the web as the task's error.
type InsufficientDiskError struct {
	Dir     string
	Need    int64 // bytes the download still needs to write
	Free    int64 // bytes currently free on Dir's filesystem
	Reserve int64 // bytes to keep free after the download
}

func (e *InsufficientDiskError) Error() string {
	return fmt.Sprintf(
		"insufficient disk space in %s: need %s + %s reserve, only %s free",
		e.Dir, formatBytes(e.Need), formatBytes(e.Reserve), formatBytes(e.Free),
	)
}

// IsInsufficientDisk reports whether err is (or wraps) an InsufficientDiskError.
func IsInsufficientDisk(err error) bool {
	var d *InsufficientDiskError
	return errors.As(err, &d)
}

// CheckDiskSpace fails fast when dir's filesystem can't hold needBytes while
// keeping reserveBytes free. It's the pre-flight guard so a download never fills
// the disk to 0 mid-write (which corrupts the partial file and can wedge the OS).
//
// Best-effort by design: a non-positive needBytes (size unknown) or a failure to
// stat the filesystem returns nil rather than block a download on a guard we
// can't evaluate — the OS-level ENOSPC stays the backstop.
func CheckDiskSpace(dir string, needBytes, reserveBytes int64) error {
	if needBytes <= 0 {
		return nil // size unknown — nothing to check against
	}
	free, _, err := agent.DiskInfo(dir)
	if err != nil {
		log.Printf("[disk] free-space pre-flight skipped for %q: stat error: %v", dir, err)
		return nil
	}
	if free <= 0 {
		// Distinct from a stat error: DiskInfo succeeded but reports no free
		// space. Don't block on a value we can't trust (0/negative) — log it so a
		// genuinely-full disk is visible rather than masked as a generic skip.
		log.Printf("[disk] free-space pre-flight skipped for %q: DiskInfo reported non-positive free (%d)", dir, free)
		return nil
	}
	if free-needBytes < reserveBytes {
		return &InsufficientDiskError{Dir: dir, Need: needBytes, Free: free, Reserve: reserveBytes}
	}
	return nil
}

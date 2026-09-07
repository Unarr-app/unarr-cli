// Package fsx holds the small filesystem helpers more than one package needs.
package fsx

import (
	"os"
	"time"
)

// RenameWithRetry is os.Rename plus a bounded wait for the Windows case where
// another process momentarily holds one of the two paths — an on-access
// antivirus scan of a file written a millisecond ago, a search indexer,
// Explorer building a thumbnail, a tray polling the file on a timer. See
// IsTransientRenameBlock for what that looks like and why it is not an error
// worth surfacing on the first attempt. On every other platform this is
// exactly os.Rename: POSIX rename(2) replaces the destination atomically no
// matter who has it open, so there is nothing to wait out.
//
// window bounds the total wait, step the pause between attempts. The error
// returned is the LAST one, so a genuine permission problem still reports
// itself as a permission problem rather than as a timeout.
func RenameWithRetry(src, dst string, window, step time.Duration) error {
	deadline := time.Now().Add(window)
	for {
		err := os.Rename(src, dst)
		if err == nil || !IsTransientRenameBlock(err) || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(step)
	}
}

//go:build !windows

package library

import (
	"os"
	"syscall"
)

// diskUsage returns the REAL number of bytes a file occupies on disk, not its
// apparent size. On POSIX this is the allocated block count × 512 (the block unit
// st_blocks is always reported in, independent of the filesystem's logical block
// size). This is what `du` counts and what actually frees when the file is removed.
//
// The distinction matters for reconcile's "freed" total: preallocated / corrupt
// downloads are frequently SPARSE — a 1.1 GiB apparent .part backed by ~0 real
// blocks (holes). Reporting apparent size lied ("57.7 GB freed" when `du` dropped
// ~7 GB). Blocks*512 tells the honest story.
//
// Falls back to apparent size if the Stat_t assertion fails (a non-POSIX unix
// filesystem or an unexpected Sys() type) — better an over-estimate than a panic.
func diskUsage(info os.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int64(st.Blocks) * 512
	}
	return info.Size()
}

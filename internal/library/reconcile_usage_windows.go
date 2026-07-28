//go:build windows

package library

import "os"

// diskUsage on Windows falls back to the apparent size. Windows' os.FileInfo.Sys()
// returns a *syscall.Win32FileAttributeData, which exposes the logical file size
// but NOT the on-disk allocated size — the real allocated size needs
// GetCompressedFileSize / DeviceIoControl(FSCTL_QUERY_ALLOCATED_RANGES), which is
// beyond a best-effort stat and not worth a syscall per file in the sweep.
//
// So on Windows the "freed" total is the apparent size (best effort). Sparse files
// are far rarer on the NTFS-backed download dirs Windows agents use than on the
// NFS/ext4 NAS mounts where the sparse-download problem was observed, so the
// apparent-vs-real gap this corrects is a POSIX-side concern in practice.
func diskUsage(info os.FileInfo) int64 {
	return info.Size()
}

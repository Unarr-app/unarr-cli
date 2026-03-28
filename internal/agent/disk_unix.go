//go:build !windows

package agent

import "syscall"

// DiskInfo returns free and total bytes for the filesystem containing path.
func DiskInfo(path string) (freeBytes, totalBytes int64, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, err
	}
	// Available blocks * block size
	freeBytes = int64(stat.Bavail) * int64(stat.Bsize)
	totalBytes = int64(stat.Blocks) * int64(stat.Bsize)
	return freeBytes, totalBytes, nil
}

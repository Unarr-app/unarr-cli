//go:build !windows

package cmd

import (
	"fmt"
	"syscall"
)

func checkDiskSpace(dir string) (string, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return "", err
	}
	available := int64(stat.Bavail) * int64(stat.Bsize)
	gb := float64(available) / (1024 * 1024 * 1024)
	msg := fmt.Sprintf("%.1f GB free", gb)
	if gb < 10 {
		return "!" + msg + " (low)", nil
	}
	return msg, nil
}

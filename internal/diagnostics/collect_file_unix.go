//go:build linux || darwin || freebsd

package diagnostics

import (
	"os"
	"syscall"
)

func openDiagnosticFile(path string) (*os.File, error) {
	// O_NONBLOCK also protects against a regular file replaced by a FIFO
	// between Lstat and open. O_NOFOLLOW rejects a raced-in symbolic link.
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

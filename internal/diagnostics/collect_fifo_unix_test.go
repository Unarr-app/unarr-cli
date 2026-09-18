//go:build linux || darwin || freebsd

package diagnostics

import (
	"path/filepath"
	"syscall"
	"testing"
)

func TestLogFIFOIsRejectedWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unarr.log")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	b, status := readRegularTail(path, 1024)
	if len(b) != 0 || status != "not-regular" {
		t.Fatalf("read FIFO: %s", status)
	}
}

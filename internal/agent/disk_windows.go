//go:build windows

package agent

import (
	"syscall"
	"unsafe"
)

// DiskInfo returns free and total bytes for the filesystem containing path.
func DiskInfo(path string) (freeBytes, totalBytes int64, err error) {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getDiskFreeSpaceEx := kernel32.NewProc("GetDiskFreeSpaceExW")

	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}

	var freeBytesAvailable, totalNumberOfBytes uint64
	r1, _, e1 := getDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(unsafe.Pointer(&freeBytesAvailable)),
		uintptr(unsafe.Pointer(&totalNumberOfBytes)),
		0,
	)
	if r1 == 0 {
		return 0, 0, e1
	}
	return int64(freeBytesAvailable), int64(totalNumberOfBytes), nil
}

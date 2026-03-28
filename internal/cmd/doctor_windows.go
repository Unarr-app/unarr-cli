//go:build windows

package cmd

import (
	"fmt"
	"syscall"
	"unsafe"
)

func checkDiskSpace(dir string) (string, error) {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getDiskFreeSpaceEx := kernel32.NewProc("GetDiskFreeSpaceExW")

	var freeBytesAvailable, totalBytes, totalFreeBytes int64
	dirPtr, _ := syscall.UTF16PtrFromString(dir)
	ret, _, err := getDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(dirPtr)),
		uintptr(unsafe.Pointer(&freeBytesAvailable)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalFreeBytes)),
	)
	if ret == 0 {
		return "", fmt.Errorf("GetDiskFreeSpaceEx: %w", err)
	}

	gb := float64(freeBytesAvailable) / (1024 * 1024 * 1024)
	msg := fmt.Sprintf("%.1f GB free", gb)
	if gb < 10 {
		return "!" + msg + " (low)", nil
	}
	return msg, nil
}

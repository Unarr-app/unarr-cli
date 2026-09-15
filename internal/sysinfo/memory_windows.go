//go:build windows

package sysinfo

import "unsafe"

var procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")

// memoryStatusEx mirrors MEMORYSTATUSEX.
type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

// platformTotalMemory is the installed RAM (GlobalMemoryStatusEx).
func platformTotalMemory() (uint64, bool) {
	if err := procGlobalMemoryStatusEx.Find(); err != nil {
		return 0, false
	}
	m := memoryStatusEx{}
	m.length = uint32(unsafe.Sizeof(m))
	r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	return m.totalPhys, r != 0 && m.totalPhys > 0
}

//go:build windows

package cmd

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// launchedByTaskShim reports whether the scheduled task's launcher shim started
// this process (see shimChain). Best-effort: without a process snapshot the
// answer is "no", which leaves the shim's own relaunch budget in charge.
func launchedByTaskShim() bool {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(snap)

	procs := map[uint32]procEntry{}
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		procs[e.ProcessID] = procEntry{parent: e.ParentProcessID, exe: windows.UTF16ToString(e.ExeFile[:])}
	}
	return shimChain(procs, uint32(os.Getpid()))
}

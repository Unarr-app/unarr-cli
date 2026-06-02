//go:build linux

package mediainfo

import "syscall"

// Linux I/O priority (ioprio) constants. The 16-bit ioprio value packs a class
// in the top 3 bits (shift 13) and a class-data nibble below it; the IDLE class
// takes no data.
const (
	ioprioWhoProcess = 1 // IOPRIO_WHO_PROCESS
	ioprioClassIdle  = 3 // IOPRIO_CLASS_IDLE
	ioprioClassShift = 13
)

// setIdleIOPriority best-effort lowers a process's I/O scheduling class to IDLE,
// so a long background read (the subtitle prewarm of a huge remux — a single
// ~14 min sequential read of a 60GB file over NFS) yields disk/NFS bandwidth to
// foreground work like live streaming. Linux-only; on kernels or filesystems
// that don't honor ioprio this simply has no effect. Errors are intentionally
// ignored — it's an optimization, never required for correctness.
func setIdleIOPriority(pid int) {
	ioprio := ioprioClassIdle << ioprioClassShift // IDLE class, data 0
	_, _, _ = syscall.Syscall(syscall.SYS_IOPRIO_SET, uintptr(ioprioWhoProcess), uintptr(pid), uintptr(ioprio))
}

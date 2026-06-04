//go:build linux

package mediainfo

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

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

// setLowCPUPriority best-effort drops a process to the lowest CPU niceness (19),
// so the heavy trickplay full-decode pass yields the CPU to foreground work.
// Pairs with setIdleIOPriority (disk): IDLE I/O alone is not enough when the
// bottleneck is software/contended 4K decode — without CPU nice, N stacked
// decodes pin every core (the host hit load ~140). Errors are ignored — it's an
// optimization, not required for correctness.
func setLowCPUPriority(pid int) {
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, pid, 19)
}

// hardenCmd makes the child ffmpeg die with this agent. Setpgid isolates it in
// its own process group, and Pdeathsig=SIGKILL asks the kernel to kill it the
// instant the agent process dies. Without this, exec.CommandContext can only
// enforce its timeout from an in-process goroutine — an agent crash / restart /
// SIGKILL kills that goroutine, so the ffmpeg is reparented to init (ppid 1) and
// runs its full 45-min decode to the end. Successive dev restarts stacked those
// orphans (one pair per restart) and spiked the box to load ~140.
func hardenCmd(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}

// LoadAverage1 returns the 1-minute system load from /proc/loadavg. ok=false when
// it can't be read, so callers treat "unknown" as "don't gate" (proceed) rather
// than blocking forever.
func LoadAverage1() (float64, bool) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

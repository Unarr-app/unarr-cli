package agent

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// Bounded free-space probing.
//
// DiskInfo is a bare statfs(2) / GetDiskFreeSpaceExW call: it takes no context,
// honours no deadline, and on a download dir that lives on an unreachable
// network share (SMB/DFS/NFS whose server went away) it does not return. Ever.
//
// That matters because two daemon-lifetime paths call it: register(), which
// runs during startup before the daemon is serving anything, and the sync
// heartbeat, which runs for as long as the agent is up. A single wedged statfs
// there takes the whole agent with it — silently, since neither call site logs
// on the way in. Free space is a nice-to-have field on a heartbeat; it must
// never be able to stop the agent from starting.
//
// So probes run off the calling goroutine with a deadline. The subtlety is that
// a goroutine blocked in an uninterruptible syscall cannot be cancelled — it
// pins an OS thread until the kernel gives up, which on a dead mount may be
// never. Spawning one per heartbeat would therefore leak a thread every few
// seconds. Hence single-flight: at most ONE outstanding probe per path, and
// while it is outstanding every caller is served instantly from the last good
// sample (or told there is none). Worst case is one parked thread per path,
// bounded and constant.

// diskProbeTimeout is how long a caller waits for free-space numbers before
// giving up on them. Generous for any local filesystem, short enough that a
// dead mount costs the daemon one wait and nothing more. A var, not a const, so
// tests can shrink it — the behaviour under test is the giving-up, not the wait.
var diskProbeTimeout = 5 * time.Second

// diskInfoFn is the syscall seam, overridable for testing (mirrors
// stateFilePathFn). The whole point of this file is what happens when the real
// call never returns, which no real filesystem will do on demand.
var diskInfoFn = DiskInfo

type diskSample struct {
	free, total int64
	valid       bool
}

var (
	diskProbeMu   sync.Mutex
	diskProbeBusy = map[string]bool{}
	diskProbeLast = map[string]diskSample{}
	diskProbeSlow = map[string]bool{} // path → "we already logged that this is stuck"
)

// resetDiskProbeState clears the per-path caches. Test-only: probe state is
// process-lifetime by design (a stuck mount stays stuck), so tests that share a
// path would otherwise see each other's single-flight and cached samples.
func resetDiskProbeState() {
	diskProbeMu.Lock()
	defer diskProbeMu.Unlock()
	diskProbeBusy = map[string]bool{}
	diskProbeLast = map[string]diskSample{}
	diskProbeSlow = map[string]bool{}
}

// DiskInfoBounded reports free/total bytes for path, giving up after
// diskProbeTimeout instead of blocking forever on an unreachable mount.
//
// On timeout it returns the most recent successful sample for that path when
// there is one, and an error when there is not. Callers already treat an error
// as "omit the disk figures" (`if free, total, err := …; err == nil`), so a
// slow mount costs the report its free-space fields and nothing else.
func DiskInfoBounded(path string) (free, total int64, err error) {
	diskProbeMu.Lock()
	if diskProbeBusy[path] {
		// A previous probe is still parked in the syscall. Starting another would
		// leak a second thread against the same dead mount and learn nothing.
		last := diskProbeLast[path]
		diskProbeMu.Unlock()
		if last.valid {
			return last.free, last.total, nil
		}
		return 0, 0, fmt.Errorf("disk probe for %s is stuck (no earlier reading)", path)
	}
	diskProbeBusy[path] = true
	// Snapshot the seam under the same lock that publishes it. The probe
	// goroutine below can outlive this call by an unbounded amount (that is the
	// whole point — it is parked in a syscall nobody can cancel), so it must
	// never read a package-level var that something else may write meanwhile.
	probe, timeout := diskInfoFn, diskProbeTimeout
	diskProbeMu.Unlock()

	// Buffered so this goroutine can always deliver and exit, even after the
	// caller below has stopped waiting.
	done := make(chan diskSample, 1)
	go func() {
		f, t, derr := probe(path)
		diskProbeMu.Lock()
		diskProbeBusy[path] = false
		if derr == nil {
			diskProbeLast[path] = diskSample{free: f, total: t, valid: true}
		}
		if diskProbeSlow[path] {
			delete(diskProbeSlow, path)
			log.Printf("[disk] free-space probe for %s responded again", path)
		}
		diskProbeMu.Unlock()
		done <- diskSample{free: f, total: t, valid: derr == nil}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case s := <-done:
		if !s.valid {
			return 0, 0, fmt.Errorf("disk info for %s", path)
		}
		return s.free, s.total, nil
	case <-timer.C:
		diskProbeMu.Lock()
		last := diskProbeLast[path]
		first := !diskProbeSlow[path]
		diskProbeSlow[path] = true
		diskProbeMu.Unlock()
		if first {
			// Say it once. A stuck mount stays stuck, and the heartbeat would
			// otherwise repeat this line for as long as the agent runs.
			log.Printf("[disk] free-space probe for %s did not answer in %s "+
				"(unreachable network share?) — continuing without disk figures", path, diskProbeTimeout)
		}
		if last.valid {
			return last.free, last.total, nil
		}
		return 0, 0, fmt.Errorf("disk probe for %s timed out", path)
	}
}

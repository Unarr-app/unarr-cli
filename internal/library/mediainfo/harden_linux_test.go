//go:build linux

package mediainfo

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestHardenCmd_KillsChildOnParentDeath is the e2e guarantee for the orphan fix:
// a child spawned with hardenCmd must be SIGKILL'd by the kernel the instant its
// parent process dies (Pdeathsig), so an agent crash/restart can never leave an
// ffmpeg running to ppid 1. It re-execs this test binary as a short-lived helper
// that starts `sleep`, prints the sleep PID, then exits — and asserts that PID is
// gone afterwards.
func TestHardenCmd_KillsChildOnParentDeath(t *testing.T) {
	if os.Getenv("UNARR_PDEATHSIG_CHILD") == "1" {
		// Helper role: start a hardened long sleep, announce its PID, then exit so
		// the kernel fires Pdeathsig on it.
		cmd := exec.Command("sleep", "120")
		hardenCmd(cmd)
		if err := cmd.Start(); err != nil {
			fmt.Println("ERR", err)
			os.Exit(2)
		}
		fmt.Println(cmd.Process.Pid)
		os.Exit(0)
	}

	helper := exec.Command(os.Args[0], "-test.run=TestHardenCmd_KillsChildOnParentDeath", "-test.v")
	helper.Env = append(os.Environ(), "UNARR_PDEATHSIG_CHILD=1")
	out, err := helper.Output()
	if err != nil {
		t.Fatalf("helper run: %v (out=%q)", err, out)
	}

	var sleepPID int
	for _, line := range strings.Split(string(out), "\n") {
		if n, perr := strconv.Atoi(strings.TrimSpace(line)); perr == nil && n > 0 {
			sleepPID = n
			break
		}
	}
	if sleepPID == 0 {
		t.Fatalf("could not parse child PID from helper output: %q", out)
	}

	// Give the kernel a moment to deliver SIGKILL after the helper exited.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(sleepPID, 0) != nil {
			return // process gone → Pdeathsig worked
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Cleanup if it somehow survived, then fail.
	_ = syscall.Kill(sleepPID, syscall.SIGKILL)
	t.Fatalf("child %d survived parent death — Pdeathsig not applied (orphan leak)", sleepPID)
}

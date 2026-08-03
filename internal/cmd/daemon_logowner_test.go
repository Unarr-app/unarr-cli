package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/logging"
)

// deadPID is the repo's stand-in for a process that is gone (see
// agent.TestIsProcessAliveBogus): far above any PID an OS hands out.
const deadPID = 0x7FFFFFFE

// ownedLogFixture sets up the shape every test here needs: a redirected data
// dir, a 1 MiB ring budget, an over-budget unarr.log and a FULL history ring.
// It returns the data dir and the history the ring must still hold afterwards.
func ownedLogFixture(t *testing.T) (string, []string) {
	t.Helper()
	dir := withDataDir(t)

	cfg := config.Default()
	cfg.Daemon.LogMaxSizeMB = 1
	cfg.Daemon.LogMaxFiles = 3
	withConfig(t, cfg)

	path := filepath.Join(dir, logFileName)
	if err := os.WriteFile(path, []byte(strings.Repeat("o", 2*1024*1024)), 0o644); err != nil {
		t.Fatalf("seed log: %v", err)
	}
	history := make([]string, 4)
	for i := 1; i <= 3; i++ {
		history[i] = "history slot " + string(rune('0'+i))
		if err := os.WriteFile(logging.RotatedPath(path, i), []byte(history[i]), 0o644); err != nil {
			t.Fatalf("seed slot %d: %v", i, err)
		}
	}
	return dir, history
}

// writeOwnerState puts a daemon state file on disk claiming the daemon log.
// It writes the JSON directly rather than going through agent.WriteState,
// because WriteState stamps the claim of the CURRENT process — and this test
// process owns nothing.
func writeOwnerState(t *testing.T, dir string, pid int, heartbeat time.Time) {
	t.Helper()
	st := agent.DaemonState{
		Status:        "running",
		Version:       "1.0.0",
		PID:           pid,
		StartedAt:     time.Now().Add(-time.Hour),
		LastHeartbeat: heartbeat,
		LogFile:       filepath.Join(dir, logFileName),
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if err := os.WriteFile(agent.StateFilePath(), b, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
}

// assertHistoryKept fails when the ring moved.
func assertHistoryKept(t *testing.T, dir string, history []string) {
	t.Helper()
	path := filepath.Join(dir, logFileName)
	for i := 1; i < len(history); i++ {
		b, err := os.ReadFile(logging.RotatedPath(path, i))
		if err != nil || string(b) != history[i] {
			t.Fatalf("unarr.log.%d is %q (err %v), want %q", i, string(b), err, history[i])
		}
	}
}

// TestExternalRotationStandsDownForALiveOwner is HIGH-2. The file is perfectly
// truncatable — a Go owner grants FILE_SHARE_WRITE on Windows and locks nothing
// on POSIX, so no probe can see it — and `unarr self-update` rotated it anyway,
// on the Windows path that runs unattended: reregisterWindowsTaskAfterUpgrade
// trims the log BEFORE the daemon is restarted, while the old daemon is still
// writing to it. Only an explicit claim can stop that.
func TestExternalRotationStandsDownForALiveOwner(t *testing.T) {
	dir, history := ownedLogFixture(t)
	writeOwnerState(t, dir, os.Getpid(), time.Now())

	err := logging.RotateNow(logRingOptions(filepath.Join(dir, logFileName)))
	if !errors.Is(err, logging.ErrOwnedByLiveProcess) {
		t.Fatalf("external rotation returned %v, want ErrOwnedByLiveProcess", err)
	}

	// The installers' and self-update's entry point must be a plain no-op.
	rotateDaemonLogIn(dir)
	if got := mustFileSize(t, filepath.Join(dir, logFileName)); got != 2*1024*1024 {
		t.Fatalf("live log is %d bytes: the owner's file was trimmed underneath it", got)
	}
	assertHistoryKept(t, dir, history)
}

// TestLogsRotateExplainsItselfUnderALiveOwner: a user who runs `unarr logs
// rotate` while the daemon is up must be told what is happening. Silence would
// read as "it worked", and an error would read as "unarr is broken", when the
// truth is that the daemon rotates that file itself.
func TestLogsRotateExplainsItselfUnderALiveOwner(t *testing.T) {
	dir, _ := ownedLogFixture(t)
	writeOwnerState(t, dir, os.Getpid(), time.Now())

	var err error
	out := captureStdout(t, func() { err = runLogsRotate() })
	if err != nil {
		t.Fatalf("runLogsRotate returned %v, want a clean explanation", err)
	}
	for _, want := range []string{logFileName, "daemon", "rotates"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the explanation does not mention %q:\n%s", want, out)
		}
	}
}

// TestExternalRotationProceedsOnAStaleStateFile: a crashed daemon leaves its
// state file behind, PID and all. If a leftover claim blocked rotation, the log
// would grow without a ceiling until someone noticed — so liveness, not the
// claim alone, decides.
func TestExternalRotationProceedsOnAStaleStateFile(t *testing.T) {
	dir, _ := ownedLogFixture(t)
	writeOwnerState(t, dir, deadPID, time.Now())

	if err := logging.RotateNow(logRingOptions(filepath.Join(dir, logFileName))); err != nil {
		t.Fatalf("a claim from a dead PID must not block rotation: %v", err)
	}
	if got := mustFileSize(t, filepath.Join(dir, logFileName)); got != 0 {
		t.Fatalf("live log is %d bytes, want it rotated", got)
	}
}

// TestExternalRotationProceedsWhenTheDaemonWentSilent: same reasoning for the
// other half of isDaemonAlive — a PID the OS still has but a heartbeat that
// stopped long ago is the CLI's definition of "not running", and rotation must
// follow that one definition rather than inventing a second.
func TestExternalRotationProceedsWhenTheDaemonWentSilent(t *testing.T) {
	dir, _ := ownedLogFixture(t)
	writeOwnerState(t, dir, os.Getpid(), time.Now().Add(-time.Hour))

	if err := logging.RotateNow(logRingOptions(filepath.Join(dir, logFileName))); err != nil {
		t.Fatalf("a long-silent daemon must not block rotation: %v", err)
	}
	if got := mustFileSize(t, filepath.Join(dir, logFileName)); got != 0 {
		t.Fatalf("live log is %d bytes, want it rotated", got)
	}
}

// TestExternalRotationIgnoresAClaimOnAnotherFile: a daemon owning unarr.log
// says nothing about the boot log, which the supervisor holds and the janitor
// still has to trim.
func TestExternalRotationIgnoresAClaimOnAnotherFile(t *testing.T) {
	dir, _ := ownedLogFixture(t)
	writeOwnerState(t, dir, os.Getpid(), time.Now())

	boot := filepath.Join(dir, bootLogFileName)
	if err := os.WriteFile(boot, make([]byte, bootLogMaxBytes+1), 0o644); err != nil {
		t.Fatalf("seed boot log: %v", err)
	}
	if err := logging.RotateNow(bootLogRingOptions(boot)); err != nil {
		t.Fatalf("rotate boot log: %v", err)
	}
	if got := mustFileSize(t, boot); got != 0 {
		t.Fatalf("boot log is %d bytes, want it rotated — the daemon never claimed it", got)
	}
}

// TestClaimIsVisibleBeforeRegistration: the claim has to be on disk from the
// moment the Writer takes the file, not from the first heartbeat. Registration
// needs the network, and a daemon parked offline writes to its log for hours
// before it ever publishes a registered state.
func TestClaimIsVisibleBeforeRegistration(t *testing.T) {
	dir, _ := ownedLogFixture(t)
	path := filepath.Join(dir, logFileName)

	agent.ClaimLogFile(path, "9.9.9")
	t.Cleanup(func() {
		agent.ReleaseLogFile()
		agent.RemoveState()
	})

	st := agent.ReadState()
	if st == nil {
		t.Fatal("no state file: the claim was not published")
	}
	if st.LogFile != path || st.PID != os.Getpid() {
		t.Fatalf("claim is %+v, want this process owning %s", st, path)
	}
	if _, ok := daemonLogOwner(path); !ok {
		t.Fatal("daemonLogOwner does not see the claim it was just handed")
	}
}

// mustFileSize fails when path is missing.
func mustFileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}

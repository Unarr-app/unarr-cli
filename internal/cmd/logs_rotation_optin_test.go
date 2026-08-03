package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/logging"
)

// oversizedLog is a log far past every budget in the tree: past the 2 MB the
// boot log uses once rotation is on, and past anything a test configures. A
// rotator that is awake at all will fire on a file this size.
const oversizedLog = 3 * 1024 * 1024

// rotationDriver is one entry point that can rotate a log, named so a failure
// says which one woke up.
type rotationDriver struct {
	name string
	run  func(t *testing.T, dir string)
}

// rotationDrivers is the COMPLETE set of rotation entry points in the binary.
// The point of listing them here is that "rotation is off by default" is
// checked against the whole set rather than against the one or two anybody
// remembers — a new rotator added without an entry is a rotator nothing pins.
//
// Not in the list, deliberately: the VBScript shim's boot-log trim, which is
// generated text rather than a filesystem call and is pinned separately by
// TestLauncherVBSEmitsNoBootTrimWhenRotationIsOff.
func rotationDrivers() []rotationDriver {
	return []rotationDriver{
		{"logging.RotateNow on the daemon log", func(t *testing.T, dir string) {
			if err := logging.RotateNow(logRingOptions(filepath.Join(dir, logFileName))); err != nil {
				t.Fatalf("RotateNow: %v", err)
			}
		}},
		{"rotateDaemonLog (unarr daemon start)", func(t *testing.T, dir string) {
			rotateDaemonLog()
		}},
		{"rotateDaemonLogIn (installLaunchd, writeAndCreateWindowsTask)", func(t *testing.T, dir string) {
			rotateDaemonLogIn(dir)
		}},
		{"unarr logs rotate", func(t *testing.T, dir string) {
			if err := runLogsRotate(); err != nil {
				t.Fatalf("runLogsRotate: %v", err)
			}
		}},
		{"rotateBootLogIn (installLaunchd, writeAndCreateWindowsTask)", func(t *testing.T, dir string) {
			rotateBootLogIn(dir)
		}},
		{"logging.OpenFile on the boot log (startDaemonDetached)", func(t *testing.T, dir string) {
			f, err := logging.OpenFile(bootLogRingOptions(filepath.Join(dir, bootLogFileName)))
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			_ = f.Close()
		}},
		{"logging.Sweep via startLogJanitors (the daemon's janitor)", func(t *testing.T, dir string) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			startLogJanitors(ctx, time.Millisecond, "")
			// Long enough for many ticks of a janitor that had a budget to act on.
			time.Sleep(150 * time.Millisecond)
		}},
		{"logging.Writer, the owner's rename (installDaemonLogWriter)", func(t *testing.T, dir string) {
			path := filepath.Join(dir, logFileName)
			w, err := logging.NewWriter(logRingOptions(path))
			if err != nil {
				t.Fatalf("NewWriter: %v", err)
			}
			defer w.Close()
			// The Writer seeds its counter from the file on disk, so this one
			// line is already miles past any budget it might have had.
			if _, err := w.Write([]byte("one line past the budget\n")); err != nil {
				t.Fatalf("Write: %v", err)
			}
		}},
	}
}

// TestNothingRotatesWithTheDefaultConfig is the net under the descope: with
// log_max_size_mb at its default, an enormous log must survive EVERY rotation
// path untouched — no ring shifted, no live file emptied, no staging file left
// behind.
//
// It matters more than a unit test of each rotator because the failure it
// guards is a path that was never wired to the switch: one rotator with its own
// baked threshold is enough to keep the whole class of "mutate the ring before
// the operation on the live file is confirmed" alive on a default install.
func TestNothingRotatesWithTheDefaultConfig(t *testing.T) {
	for _, d := range rotationDrivers() {
		t.Run(d.name, func(t *testing.T) {
			dir := withDataDir(t)
			withConfig(t, config.Default())

			seedOversizedLogs(t, dir)
			d.run(t, dir)

			for _, name := range []string{logFileName, errLogFileName, bootLogFileName} {
				assertNotRotated(t, filepath.Join(dir, name))
			}
		})
	}
}

// The other half of the descope: rotation is DISABLED, not removed. A user who
// opts in still gets a working ring — from the outside (RotateNow, which is
// what the janitor and the installers' trim drive) and from the inside (the
// owner's Writer, which is what a running daemon uses).
func TestAnExplicitBudgetStillRotates(t *testing.T) {
	t.Run("RotateNow, from the outside", func(t *testing.T) {
		dir := withDataDir(t)
		withConfig(t, rotatingConfig())

		path := filepath.Join(dir, logFileName)
		seedOversizedLogs(t, dir)
		if err := logging.RotateNow(logRingOptions(path)); err != nil {
			t.Fatalf("RotateNow: %v", err)
		}
		assertRotated(t, path)
	})

	t.Run("Writer, from the inside", func(t *testing.T) {
		dir := withDataDir(t)
		withConfig(t, rotatingConfig())

		path := filepath.Join(dir, logFileName)
		seedOversizedLogs(t, dir)
		w, err := logging.NewWriter(logRingOptions(path))
		if err != nil {
			t.Fatalf("NewWriter: %v", err)
		}
		if _, err := w.Write([]byte("this line crosses the budget\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		assertRotated(t, path)
	})

	t.Run("the boot log gets its own fixed budget back", func(t *testing.T) {
		dir := withDataDir(t)
		withConfig(t, rotatingConfig())

		path := filepath.Join(dir, bootLogFileName)
		seedOversizedLogs(t, dir)
		if got := bootLogRingOptions(path).MaxSizeMB; got != bootLogMaxSizeMB {
			t.Fatalf("boot log budget = %d MB, want the fixed %d MB once rotation is on",
				got, bootLogMaxSizeMB)
		}
		rotateBootLogIn(dir)
		assertRotated(t, path)
	})
}

// The boot log is bounded by the VBScript shim on Windows and by nothing else —
// cmd.exe holds it for the whole run and grants only FILE_SHARE_READ. Its
// threshold is therefore baked into the generated script at install time, which
// makes it the one rotator that cannot consult the config later: if it kept its
// own 2 MB constant it would be the single ring still mutating on a default
// install.
func TestLauncherVBSEmitsNoBootTrimWhenRotationIsOff(t *testing.T) {
	withConfig(t, config.Default())
	if got := bootLogTrimBytes(); got != 0 {
		t.Fatalf("bootLogTrimBytes() = %d with rotation off, want 0", got)
	}

	off := buildLauncherVBS(`C:\unarr\unarr.exe`, `C:\unarr`, bootLogTrimBytes())
	for _, marker := range []string{"MoveFile", ".rotating", bootLogFileName + `.1`} {
		if strings.Contains(off, marker) {
			t.Errorf("the shim still rotates the boot log with rotation off: found %q in\n%s", marker, off)
		}
	}
	// It must still LAUNCH and still redirect — only the trim goes away.
	if !strings.Contains(off, bootLogFileName) || !strings.Contains(off, "sh.Run") {
		t.Fatalf("the shim lost its launch or its redirect:\n%s", off)
	}

	withConfig(t, rotatingConfig())
	if got := bootLogTrimBytes(); got != bootLogMaxBytes {
		t.Fatalf("bootLogTrimBytes() = %d with rotation on, want %d", got, bootLogMaxBytes)
	}
	on := buildLauncherVBS(`C:\unarr\unarr.exe`, `C:\unarr`, bootLogTrimBytes())
	if !strings.Contains(on, ".rotating") || !strings.Contains(on, bootLogFileName+`.1`) {
		t.Fatalf("the shim lost its boot-log trim with rotation on:\n%s", on)
	}
}

// rotatingConfig is a config that has opted into rotation, at a budget every
// seeded log is already over.
func rotatingConfig() config.Config {
	cfg := config.Default()
	cfg.Daemon.LogMaxSizeMB = 1
	cfg.Daemon.LogMaxFiles = 3
	return cfg
}

// seedOversizedLogs writes an over-budget copy of every log the daemon
// supervises, so a driver that touches any of them is caught.
func seedOversizedLogs(t *testing.T, dir string) {
	t.Helper()
	body := make([]byte, oversizedLog)
	for i := range body {
		body[i] = 'x'
	}
	for _, name := range []string{logFileName, errLogFileName, bootLogFileName} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
}

// assertNotRotated fails if path shrank, if any rotated slot appeared, or if a
// staging file was left behind.
func assertNotRotated(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	// ">=", not "==": one driver is the Writer, which legitimately APPENDS to
	// the file. What must never happen is the file getting smaller — that is
	// what a rename or a copy-truncate would look like from here.
	if fi.Size() < oversizedLog {
		t.Errorf("%s shrank to %d bytes from %d — something rotated it with rotation off",
			path, fi.Size(), oversizedLog)
	}
	for _, slot := range logging.RotatedPaths(path, logging.DefaultMaxFiles) {
		if _, err := os.Stat(slot); err == nil {
			t.Errorf("%s exists — a ring was shifted with rotation off", slot)
		}
	}
	if _, err := os.Stat(path + ".rotating"); err == nil {
		t.Errorf("%s exists — a rotation started with rotation off", path+".rotating")
	}
}

// assertRotated fails unless the live file was emptied and its contents landed
// in slot 1 — the whole point of rotation, checked so the "still works" half of
// this file cannot pass on a no-op.
func assertRotated(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if fi.Size() >= oversizedLog {
		t.Errorf("%s is still %d bytes — it was not rotated", path, fi.Size())
	}
	slot := logging.RotatedPath(path, 1)
	rotated, err := os.Stat(slot)
	if err != nil {
		t.Fatalf("stat %s: %v — the rotated contents went nowhere", slot, err)
	}
	if rotated.Size() < oversizedLog {
		t.Errorf("%s holds %d bytes, want at least the %d that were rotated out",
			slot, rotated.Size(), oversizedLog)
	}
}

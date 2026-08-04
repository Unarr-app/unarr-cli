package cmd

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/logging"
)

// withDataDir redirects every platform's data-dir resolver into a temp
// directory and returns the resolved dir, so a test can create the very files
// the daemon would write without touching the developer's own agent.
func withDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("UNARR_CONFIG_DIR", dir) // macOS: data dir == config dir
	t.Setenv("XDG_DATA_HOME", dir)    // linux
	t.Setenv("LOCALAPPDATA", dir)     // windows

	got := config.DataDir()
	if !strings.HasPrefix(got, dir) {
		t.Skipf("config.DataDir() = %s on %s, not redirected into %s", got, runtime.GOOS, dir)
	}
	if err := os.MkdirAll(got, 0o755); err != nil {
		t.Fatalf("create data dir: %v", err)
	}
	return got
}

// The janitor is the only thing that bounds a 24/7 install, and unarr.log is
// not the only file it has to bound: a macOS agent installed by a build whose
// plist split StandardErrorPath keeps sending every log.Printf — i.e. the whole
// log — to unarr.err.log, which nothing rotated. The boot log is the third:
// launchd and the detached parent hold it for the whole run and never trim it
// themselves. `clean` already sweeps all three names and their rotated copies;
// supervising only part of the set meant the file that actually grows was the
// unsupervised one.
func TestStartLogJanitorsRotatesEveryDaemonLog(t *testing.T) {
	dir := withDataDir(t)

	cfg := config.Default()
	cfg.Daemon.LogMaxSizeMB = 1
	cfg.Daemon.LogMaxFiles = 2
	withConfig(t, cfg)

	// Over BOTH budgets: 1 MiB for the configured ring, bootLogMaxBytes for the
	// boot log's own fixed one.
	overBudget := make([]byte, bootLogMaxBytes+1)
	for _, name := range []string{logFileName, errLogFileName, bootLogFileName} {
		if err := os.WriteFile(filepath.Join(dir, name), overBudget, 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startLogJanitors(ctx, 5*time.Millisecond, "")

	for _, name := range []string{logFileName, errLogFileName, bootLogFileName} {
		waitRotated(t, filepath.Join(dir, name))
	}
}

// Two rotators on one path fight: the Writer renames the live file aside while
// a copy-truncate janitor copies it and truncates it in place, so the ring ends
// up holding a duplicate of one run and losing another. The daemon's own log is
// therefore excluded from the sweep for exactly as long as it owns it — this is
// the invariant that makes rename rotation safe to turn on at all.
func TestStartLogJanitorsSkipsTheLogTheDaemonOwns(t *testing.T) {
	dir := withDataDir(t)

	cfg := config.Default()
	cfg.Daemon.LogMaxSizeMB = 1
	cfg.Daemon.LogMaxFiles = 2
	withConfig(t, cfg)

	owned := filepath.Join(dir, logFileName)
	overBudget := make([]byte, bootLogMaxBytes+1)
	for _, name := range []string{logFileName, bootLogFileName} {
		if err := os.WriteFile(filepath.Join(dir, name), overBudget, 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startLogJanitors(ctx, 5*time.Millisecond, owned)

	// The boot log still gets swept — proving the janitors really did run, so a
	// silent no-op cannot pass this test.
	waitRotated(t, filepath.Join(dir, bootLogFileName))

	fi, err := os.Stat(owned)
	if err != nil {
		t.Fatalf("stat owned log: %v", err)
	}
	if fi.Size() != int64(len(overBudget)) {
		t.Errorf("the janitor copy-truncated %s (now %d bytes, was %d) — the daemon owns it and rotates it by rename",
			owned, fi.Size(), len(overBudget))
	}
	if _, err := os.Stat(logging.RotatedPath(owned, 1)); err == nil {
		t.Errorf("the janitor shifted the ring of %s, which the daemon's Writer owns", owned)
	}
}

// The boot log holds banners and stack traces. Someone who raised the main log
// to 500 MB did not ask for a 500 MB banner file, so its budget is fixed and
// independent of [daemon] log_max_size_mb.
func TestBootLogRingIgnoresTheConfiguredBudget(t *testing.T) {
	cfg := config.Default()
	cfg.Daemon.LogMaxSizeMB = 500
	cfg.Daemon.LogMaxFiles = 9
	withConfig(t, cfg)

	got := logJanitorOptions(filepath.Join("data", bootLogFileName))
	if got.MaxSizeMB != bootLogMaxSizeMB || got.MaxFiles != bootLogMaxFiles {
		t.Errorf("boot log ring = %d MB / %d files, want the fixed %d MB / %d files",
			got.MaxSizeMB, got.MaxFiles, bootLogMaxSizeMB, bootLogMaxFiles)
	}
	// …while everything else still follows the config.
	if main := logJanitorOptions(filepath.Join("data", logFileName)); main.MaxSizeMB != 500 {
		t.Errorf("daemon log ring = %d MB, want the configured 500", main.MaxSizeMB)
	}
}

// waitRotated blocks until path has been copy-truncated (emptied, with its
// contents in slot 1), or fails the test.
func waitRotated(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		fi, err := os.Stat(path)
		if err == nil && fi.Size() == 0 {
			if _, err := os.Stat(logging.RotatedPath(path, 1)); err == nil {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s is still over budget — no janitor supervises it", path)
}

// The detached start is the third supervisor, and the one with no config file
// to get this wrong in: the child must own unarr.log (so it can rename-rotate
// it) while the inherited descriptor carries only the boot output. Without
// --log-file the daemon would log to a descriptor this short-lived parent
// opened, and nothing would ever shrink it.
func TestDetachedStartHandsTheDaemonItsOwnLog(t *testing.T) {
	args := detachedStartArgs(filepath.Join("data", logFileName))

	if len(args) == 0 || args[0] != "start" {
		t.Fatalf("detachedStartArgs() = %v, want it to run `start`", args)
	}
	if !slices.Contains(args, "--log-file") {
		t.Errorf("detachedStartArgs() = %v — the daemon is never told to own its log", args)
	}
	if slices.Contains(args, filepath.Join("data", bootLogFileName)) {
		t.Errorf("detachedStartArgs() = %v — the daemon must not own the file the parent's descriptor holds", args)
	}
}

// `clean` and the janitor must cover exactly the same set. A file one of them
// knows about and the other does not is a file that survives a clean and grows
// without a ceiling — and the boot log is the easy one to miss, because the
// existing `unarr.log.*` glob matches unarr.log.1 but never unarr.boot.log.
func TestCleanSweepsEveryLogTheJanitorSupervises(t *testing.T) {
	dir := withDataDir(t)

	targets := logCleanTargets(dir)
	for _, path := range daemonLogPaths() {
		live, rotated := false, false
		for _, target := range targets {
			switch target.path {
			case path:
				live = true
			case path + ".*":
				rotated = true
			}
		}
		if !live {
			t.Errorf("`clean` does not remove %s, which the janitor supervises", path)
		}
		if !rotated {
			t.Errorf("`clean` does not remove the rotated ring of %s", path)
		}
	}
}

// An installer holds the directory whose log the service manager is about to
// open, and that is not always the machine's data dir. Resolving the global one
// instead would copy-truncate a file nobody asked about — including, when a test
// drives an installer without redirecting the data dir, the developer's own live
// daemon log, shifting its ring and discarding the oldest slot.
func TestRotateDaemonLogInLeavesOtherDirsAlone(t *testing.T) {
	dataDir := withDataDir(t)
	installDir := t.TempDir()

	oversized := strings.Repeat("x", 2<<20) // 2 MiB, over the 1 MiB budget below
	t.Setenv("UNARR_LOG_MAX_SIZE_MB", "1")
	for _, dir := range []string{dataDir, installDir} {
		if err := os.WriteFile(filepath.Join(dir, logFileName), []byte(oversized), 0o644); err != nil {
			t.Fatalf("seed %s: %v", dir, err)
		}
	}

	rotateDaemonLogIn(installDir)

	// The machine's own log must be byte-for-byte untouched.
	fi, err := os.Stat(filepath.Join(dataDir, logFileName))
	if err != nil {
		t.Fatalf("stat data-dir log: %v", err)
	}
	if fi.Size() != int64(len(oversized)) {
		t.Errorf("rotateDaemonLogIn(%s) truncated the data-dir log: %d bytes, want %d",
			installDir, fi.Size(), len(oversized))
	}
	if _, err := os.Stat(logging.RotatedPath(filepath.Join(dataDir, logFileName), 1)); err == nil {
		t.Error("rotateDaemonLogIn shifted the data-dir ring; it must only touch the dir it was given")
	}
}

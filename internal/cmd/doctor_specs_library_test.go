package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

func TestLibraryDirsDedupesAndSkipsUnset(t *testing.T) {
	cfg := config.Default()
	cfg.Organize.MoviesDir = "/srv/media"
	cfg.Organize.TVShowsDir = "/srv/media" // the same directory, a normal setup
	if got := libraryDirs(&cfg); len(got) != 1 {
		t.Errorf("one directory configured twice must be probed once, got %v", got)
	}

	cfg.Organize.TVShowsDir = "  " // whitespace is not a path
	if got := libraryDirs(&cfg); len(got) != 1 {
		t.Errorf("a blank dir must be skipped, got %v", got)
	}

	cfg.Organize.MoviesDir, cfg.Organize.TVShowsDir = "", ""
	if got := libraryDirs(&cfg); len(got) != 0 {
		t.Errorf("nothing configured must yield nothing, got %v", got)
	}
}

// Nothing configured is the default install (organize leaves files where they
// landed). It is not a fault and must not be reported as one.
func TestLibraryDirsResultUnconfiguredIsClean(t *testing.T) {
	msg, err := libraryDirsResult(nil)
	if err != nil {
		t.Fatalf("unconfigured is not a failure: %v", err)
	}
	if strings.HasPrefix(msg, "!") {
		t.Errorf("nor a warning: %q", msg)
	}
}

func TestProbeLibraryDirOnAGoodDirectory(t *testing.T) {
	dir := t.TempDir()
	if msg := probeLibraryDir(dir); msg != "" {
		t.Fatalf("a writable temp dir must probe clean, got %q", msg)
	}
	// The probe file is temporary — a check that littered the user's library
	// with droppings would be its own bug report.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), libraryProbePrefix) {
			t.Errorf("probe file left behind: %s", e.Name())
		}
	}
	if len(entries) != 0 {
		t.Errorf("the probe left %d file(s) behind", len(entries))
	}
}

func TestProbeLibraryDirReportsMissingAndNotADirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	if msg := probeLibraryDir(missing); !strings.Contains(msg, "does not exist") {
		t.Errorf("missing dir = %q", msg)
	}

	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if msg := probeLibraryDir(file); !strings.Contains(msg, "not a directory") {
		t.Errorf("file-as-dir = %q", msg)
	}
}

func TestProbeLibraryDirReportsAnUnwritableDirectory(t *testing.T) {
	// Windows does not implement the POSIX write bit: os.Mkdir(dir, 0o500)
	// produces a directory files can still be created in, so the condition
	// cannot be set up at all. Measured on the Windows VM, where this test
	// failed with an empty finding — the harness's fault, not the check's.
	// Permission problems there present as ACL denials, which need a real ACL
	// to reproduce and are out of this test's scope.
	if runtime.GOOS == "windows" {
		t.Skip("Windows ignores the POSIX write bit; an unwritable directory needs an ACL to set up")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the write bit, so there is nothing to detect")
	}
	dir := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	msg := probeLibraryDir(dir)
	if !strings.Contains(msg, "not writable") {
		t.Errorf("expected a not-writable finding, got %q", msg)
	}
}

// probeChmodThenReopen is the step that catches NFS root_squash / SMB uid
// mapping, where the chmod reports success and the file is still unopenable.
// That condition cannot be produced on a local filesystem, so what is asserted
// here is the honest half: the ordinary path returns clean, and a file that
// vanished under it is reported rather than silently passing.
func TestProbeChmodThenReopen(t *testing.T) {
	f := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if msg := probeChmodThenReopen(f); msg != "" {
		t.Errorf("a local file must probe clean, got %q", msg)
	}

	gone := filepath.Join(t.TempDir(), "gone")
	if msg := probeChmodThenReopen(gone); msg == "" {
		t.Error("a missing file must not probe clean")
	}
}

func TestLibraryFreeSpaceReportsPerDirectory(t *testing.T) {
	dirs := []libraryDir{{key: "organize.movies_dir", path: t.TempDir()}}
	msg, err := libraryFreeSpaceResult(dirs)
	// Low disk is a WARN at most: a full library stops new downloads landing,
	// it does not break the agent, and a red doctor for "buy a bigger disk"
	// teaches people to ignore red.
	if err != nil {
		t.Fatalf("free space must never FAIL: %v", err)
	}
	if !strings.Contains(msg, "GB free") {
		t.Errorf("message does not report space: %q", msg)
	}
}

func TestLibraryDirsResultNamesTheOffendingKey(t *testing.T) {
	dirs := []libraryDir{{key: "organize.tv_shows_dir", path: filepath.Join(t.TempDir(), "missing")}}
	msg, err := libraryDirsResult(dirs)
	if err == nil {
		t.Fatalf("a missing library dir must FAIL, got %q", msg)
	}
	// The user has to know which config line to edit, not just that "a
	// directory" is wrong.
	if !strings.Contains(msg, "organize.tv_shows_dir") {
		t.Errorf("message does not name the config key: %q", msg)
	}
}

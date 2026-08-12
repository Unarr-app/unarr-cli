package upgrade

// Is there ever a moment when the binary on disk is not a runnable binary?
//
// installBinary replaces the LIVE executable in place: os.ReadFile the new
// image, os.WriteFile it over the old path. os.WriteFile is O_CREATE|O_WRONLY|
// O_TRUNC — it truncates the destination to zero and then streams the image
// back. For the whole of that write the file at the path every other component
// execs is short, and a short PE is not a loadable image.
//
// That matters here because nothing waits for the upgrade. The tray polls the
// same path on a timer (runUnarrOutput in cmd/unarr-desktop/agentctl.go, the
// status/account/control loops), so an upgrade running underneath it WILL be
// exec'd mid-write. On Windows the loader answers a truncated PE with
// STATUS_DLL_INIT_FAILED / STATUS_INVALID_IMAGE_FORMAT — 0xc0000142 — which is
// the exact status a field crash report carried, in the log tail, from the
// process the tray spawned to collect the logs.
//
// The fix these tests describe is the standard one: write a sibling temp file,
// fsync it, then rename it over the target. A rename is atomic at the directory
// entry, so a concurrent open sees either the old image or the new one.

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestRenameWithRetryDoesNotWaitOutAFailureItCannotFix is the counterfactual to
// the retry: a rename that fails for a reason no amount of waiting changes must
// come back immediately, with its own error. Without this, "retry on Windows"
// could quietly become "every broken install takes two seconds to report", and
// the message would still be right, so nothing would look wrong.
func TestRenameWithRetryDoesNotWaitOutAFailureItCannotFix(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "not-there")

	start := time.Now()
	err := renameWithRetry(missing, filepath.Join(dir, "dst"))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("renaming a source that does not exist succeeded")
	}
	if !os.IsNotExist(err.(*os.LinkError).Err) {
		t.Errorf("err = %v, want the underlying not-exist error preserved", err)
	}
	if elapsed >= renameRetryWindow {
		t.Errorf("took %v — the retry window (%v) was spent on an error that cannot clear",
			elapsed, renameRetryWindow)
	}
}

// TestInstallBinaryNeverLeavesAPartialImage watches the destination while
// installBinary runs and fails if it is ever observed shorter than the source.
//
// The observation runs from a second goroutine rather than by instrumenting the
// write, so it measures the file as another PROCESS would see it — which is the
// thing that actually breaks.
//
// The image is deliberately large: the window is proportional to the write, and
// a few bytes would open and close between two polls on a fast disk. Real unarr
// binaries are tens of MB.
func TestInstallBinaryNeverLeavesAPartialImage(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "unarr.new")
	dst := filepath.Join(dir, "unarr")

	image := make([]byte, 8<<20) // 8 MiB
	for i := range image {
		image[i] = byte(i)
	}
	if err := os.WriteFile(src, image, 0o755); err != nil {
		t.Fatal(err)
	}
	// A complete previous version already sits at the destination, exactly as on
	// an upgrade (never a fresh install).
	if err := os.WriteFile(dst, image, 0o755); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	short := make(chan int64, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			// A missing file is fine: a rename-based install can briefly have the
			// target absent, and an exec then fails cleanly with ENOENT rather than
			// loading a corrupt image. A PRESENT but SHORT file is the fault.
			fi, err := os.Stat(dst)
			if err != nil {
				continue
			}
			if n := fi.Size(); n != 0 && n < int64(len(image)) {
				select {
				case short <- n:
				default:
				}
				return
			}
		}
	}()

	if err := installBinary(src, dst); err != nil {
		t.Fatalf("installBinary: %v", err)
	}
	close(stop)
	<-done

	select {
	case n := <-short:
		t.Errorf("the destination was observed at %d bytes (image is %d) while installBinary ran.\n"+
			"Anything that exec'd it in that window got a truncated PE; on Windows the loader "+
			"answers that with 0xc0000142.", n, len(image))
	default:
	}
}

// TestInstallBinaryIsDurable: the new image must be on the platter before
// installBinary reports success.
//
// os.WriteFile returns after the last write(2) — the bytes may still be in the
// page cache. An upgrade that "succeeded" and then lost power leaves a
// zero-length or partially-zeroed executable, and the next boot has no unarr at
// all. A rename-based install fsyncs the temp file first, which is what makes
// the rename meaningful.
//
// Durability cannot be observed without pulling the power, so this asserts the
// structural precondition instead: after a successful install the destination
// holds exactly the source bytes, and no temp file is left in the directory to
// be mistaken for a binary or to fill the disk on repeated upgrades.
func TestInstallBinaryIsDurable(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "unarr.new")
	dst := filepath.Join(dir, "unarr")

	image := []byte("\x7fELF and then some payload bytes")
	if err := os.WriteFile(src, image, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installBinary(src, dst); err != nil {
		t.Fatalf("installBinary: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if string(got) != string(image) {
		t.Errorf("destination = %q, want %q", got, image)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	// Windows has no executable bit to assert on: os.Chmod there toggles the
	// read-only attribute and nothing else, so Go reports 0666 for every writable
	// file and what makes unarr.exe runnable is its extension. Measured on the CI
	// runner as "destination mode -rw-rw-rw- is not executable" — a green install
	// failing a POSIX assumption. The chmod in installBinary still runs there and
	// is still worth keeping (it clears read-only); it is only unobservable.
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("destination mode %v is not executable", fi.Mode().Perm())
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		switch e.Name() {
		case "unarr", "unarr.new":
		default:
			t.Errorf("installBinary left %q behind in the install dir", e.Name())
		}
	}
}

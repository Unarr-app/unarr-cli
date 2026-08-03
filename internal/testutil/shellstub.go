// Package testutil holds helpers shared by tests across packages.
package testutil

import (
	"runtime"
	"testing"
)

// RequireShellStubs skips the test on platforms without a POSIX shell.
//
// Several suites fake ffmpeg/ffprobe with a `#!/bin/sh` script so they can force
// a timeout or a canned output. Windows cannot execute one, so on that runner the
// test measures the harness rather than the code. The behaviour under test is
// OS-independent and stays covered by the Linux and macOS legs of the matrix.
func RequireShellStubs(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("skipping: the ffmpeg/ffprobe stub is a #!/bin/sh script, not executable on Windows")
	}
}

package cmd

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

// withLogFileFlag sets --log-file for the duration of a test and restores both
// the flag and the standard logger's output afterwards, so one test cannot
// leave log.Printf pointing at another's temp dir.
func withLogFileFlag(t *testing.T, path string) {
	t.Helper()
	prev := daemonLogFileFlag
	t.Cleanup(func() {
		daemonLogFileFlag = prev
		log.SetOutput(os.Stderr)
	})
	daemonLogFileFlag = path
}

// The whole point of the flag: with it, log.Printf goes into the file this
// process owns — a real O_APPEND descriptor it can later rotate by rename —
// instead of stderr, which on Windows is a handle cmd.exe owns and nothing can
// truncate.
func TestInstallDaemonLogWriterOwnsTheNamedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, logFileName)
	withConfig(t, config.Default())
	withLogFileFlag(t, path)

	closeLog := installDaemonLogWriter()
	log.Printf("hello from the owned log")
	closeLog()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the owned log was never created: %v", err)
	}
	if !strings.Contains(string(body), "hello from the owned log") {
		t.Errorf("log.Printf did not reach %s:\n%s", path, body)
	}
	// The run marker is the only delimiter left once rotation is by rename: the
	// banner now lands in the supervisor's boot log, so without this line a
	// rotated unarr.log gives no clue where one run ends and the next begins.
	if !strings.Contains(string(body), "starting (pid ") {
		t.Errorf("no run marker in %s:\n%s", path, body)
	}
}

// Foreground `unarr start`, Docker (`up`) and a systemd unit pass no --log-file,
// and must behave byte for byte as before: output on stdout/stderr, for the
// terminal, `docker logs` and journald respectively. Owning a file there would
// hide the log from the only tool that reads it.
func TestInstallDaemonLogWriterIsANoOpWithoutTheFlag(t *testing.T) {
	dir := t.TempDir()
	withConfig(t, config.Default())
	withLogFileFlag(t, "")

	var sink strings.Builder
	log.SetOutput(&sink)

	closeLog := installDaemonLogWriter()
	log.Printf("stays on the supervisor's stream")
	closeLog()

	if !strings.Contains(sink.String(), "stays on the supervisor's stream") {
		t.Errorf("logging was redirected away from stderr with no --log-file: %q", sink.String())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a log file was created without --log-file: %v", entries)
	}
}

// A log file that cannot be opened must not stop downloads. The Writer is a
// convenience for reading the daemon later, never a precondition for running
// it — an AV lock or a read-only data dir has to degrade to stderr, not to a
// dead agent.
func TestInstallDaemonLogWriterSurvivesAnUnopenableFile(t *testing.T) {
	dir := t.TempDir()
	// A directory where the log file should be: open() cannot succeed on it on
	// any platform, without needing root or a permission trick.
	blocked := filepath.Join(dir, logFileName)
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	withConfig(t, config.Default())
	withLogFileFlag(t, blocked)

	var sink strings.Builder
	log.SetOutput(&sink)

	closeLog := installDaemonLogWriter()
	log.Printf("still logging after a failed writer")
	closeLog()

	if !strings.Contains(sink.String(), "still logging after a failed writer") {
		t.Errorf("a failed Writer swallowed the log instead of falling back to stderr: %q", sink.String())
	}
}

// The janitor consults ownedLogPath to decide what NOT to sweep, so the answer
// has to be comparable to the paths daemonLogPaths returns.
func TestOwnedLogPathIsEmptyWithoutTheFlag(t *testing.T) {
	withLogFileFlag(t, "")
	if got := ownedLogPath(); got != "" {
		t.Errorf("ownedLogPath() = %q with no --log-file, want \"\" so nothing is excluded from the sweep", got)
	}

	withLogFileFlag(t, filepath.Join("data", "sub", "..", logFileName))
	if got, want := ownedLogPath(), filepath.Join("data", logFileName); got != want {
		t.Errorf("ownedLogPath() = %q, want the cleaned %q — the janitor compares it against a cleaned path", got, want)
	}
}

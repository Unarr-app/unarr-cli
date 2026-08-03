//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// seededRecords is how many lines the reader cases put in unarr.log. Four
// flavours cycle through it so a single file exercises every filter at once:
// severity that passes and fails --level, and text that passes and fails
// --grep.
const seededRecords = 100

// seedRecord renders record i of the reader fixture, in the shape log.Printf
// writes ("2006/01/02 15:04:05 message").
func seedRecord(i int) string {
	stamp := time.Date(2026, 8, 3, 10, 0, 0, 0, time.Local).
		Add(time.Duration(i) * time.Minute).Format("2006/01/02 15:04:05")
	switch i % 4 {
	case 0:
		return fmt.Sprintf("%s [warn] usenet: retry %d", stamp, i)
	case 1:
		return fmt.Sprintf("%s [error] usenet: giving up on segment %d", stamp, i)
	case 2:
		return fmt.Sprintf("%s [warn] engine: disk is slow, tick %d", stamp, i)
	default:
		return fmt.Sprintf("%s [info] usenet: fetched segment %d", stamp, i)
	}
}

// wantsWarnUsenet reports whether record i is what `--level warn --grep usenet`
// selects: severity at or above warn AND the word usenet in the line.
func wantsWarnUsenet(i int) bool { return i%4 == 0 || i%4 == 1 }

// seedLog writes the fixture into the sandbox's live log file.
func seedLog(t *testing.T, s sandbox, n int) {
	t.Helper()
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(seedRecord(i))
		b.WriteByte('\n')
	}
	if err := os.WriteFile(s.logPath(), []byte(b.String()), 0o644); err != nil {
		t.Fatalf("seed log: %v", err)
	}
}

// TestLogsGrepLevelLinesReturnsExactlyTheExpectedLines runs the shipped binary
// over a seeded unarr.log and pins the output byte for byte: --grep, --level
// and --lines have to compose, and text format has to echo the line as the
// daemon wrote it.
func TestLogsGrepLevelLinesReturnsExactlyTheExpectedLines(t *testing.T) {
	s := newSandbox(t)
	s.writeConfig(t, "[daemon]\nlog_max_size_mb = 1\nlog_max_files = 3\nlog_level = \"info\"\n")
	seedLog(t, s, seededRecords)

	out := runCLI(t, s, "logs", "--grep", "usenet", "--level", "warn", "--lines", "20")
	got := splitLines(out)

	var want []string
	for i := 0; i < seededRecords; i++ {
		if wantsWarnUsenet(i) {
			want = append(want, seedRecord(i))
		}
	}
	want = want[len(want)-20:] // --lines 20 keeps the newest matches

	t.Logf("seeded %d records at %s; command returned %d lines\nfirst: %s\nlast:  %s",
		seededRecords, s.logPath(), len(got), first(got), last(got))

	if len(got) != 20 {
		t.Fatalf("got %d lines, want 20:\n%s", len(got), strings.Join(got, "\n"))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d differs:\n got: %q\nwant: %q", i, got[i], want[i])
		}
	}
	for _, ln := range got {
		if strings.Contains(ln, "[info]") || strings.Contains(ln, "engine:") {
			t.Errorf("a filtered-out line leaked through: %q", ln)
		}
	}
}

// TestLogsFollowStreamsNewLinesAndExitsCleanlyOnSIGINT starts `unarr logs -f`
// against a seeded file, appends to it while the command runs, and then ends it
// the way a user does. Ctrl-C is a clean end, not an error, so the exit status
// has to be 0.
func TestLogsFollowStreamsNewLinesAndExitsCleanlyOnSIGINT(t *testing.T) {
	s := newSandbox(t)
	s.writeConfig(t, "[daemon]\nlog_max_size_mb = 1\nlog_max_files = 3\n")
	seedLog(t, s, 10)

	f := startFollow(t, s)
	defer f.kill()

	f.waitFor(t, seedRecord(9), "the seeded tail")

	appended := []string{
		"2026/08/03 12:00:00 [warn] usenet: live line one",
		"2026/08/03 12:00:01 [warn] usenet: live line two",
	}
	appendLines(t, s.logPath(), appended)
	for _, ln := range appended {
		f.waitFor(t, ln, "an appended line")
	}

	code, err := f.interrupt(3 * time.Second)
	t.Logf("follow saw %d lines; SIGINT exit code %d", f.count(), code)
	if err != nil {
		t.Fatalf("SIGINT did not end the follow: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code %d after SIGINT, want 0 (Ctrl-C is not an error)", code)
	}
}

// runCLI runs the shipped binary inside the sandbox and returns its stdout,
// failing the test on a non-zero exit.
func runCLI(t *testing.T, s sandbox, args ...string) string {
	t.Helper()
	full := append([]string{"--config", s.cfgPath}, args...)
	cmd := exec.Command(cliBinary(t), full...)
	cmd.Env = s.env()
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		var ee *exec.ExitError
		if errorsAs(err, &ee) {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("unarr %s: %v\nstderr: %s", strings.Join(args, " "), err, stderr)
	}
	return string(out)
}

// splitLines turns command output into lines, dropping the trailing empty one.
func splitLines(out string) []string {
	out = strings.TrimSuffix(out, "\n")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// appendLines appends records to a log the way a running daemon would.
func appendLines(t *testing.T, path string, lines []string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open log for append: %v", err)
	}
	defer f.Close()
	for _, ln := range lines {
		if _, err := fmt.Fprintln(f, ln); err != nil {
			t.Fatalf("append line: %v", err)
		}
	}
}

func first(s []string) string {
	if len(s) == 0 {
		return "(none)"
	}
	return s[0]
}

func last(s []string) string {
	if len(s) == 0 {
		return "(none)"
	}
	return s[len(s)-1]
}

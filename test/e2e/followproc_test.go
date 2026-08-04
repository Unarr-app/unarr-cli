//go:build e2e

package e2e

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// followTimeout is how long a line has to show up under `logs --follow`. The
// follower polls every 250 ms, so seconds of slack cover a loaded CI box
// without letting a hang look like a pass.
const followTimeout = 10 * time.Second

// errorsAs is errors.As, named locally so the assertion helpers read the same
// in every file of this package.
func errorsAs(err error, target any) bool { return errors.As(err, target) }

// followProc is a running `unarr logs --follow`, with its stdout collected in
// the background so a test can wait for a specific line to arrive.
type followProc struct {
	cmd  *exec.Cmd
	done chan error

	mu    sync.Mutex
	lines []string
}

// startFollow launches `unarr logs -f` inside the sandbox.
func startFollow(t *testing.T, s sandbox) *followProc {
	t.Helper()
	cmd := exec.Command(cliBinary(t), "--config", s.cfgPath, "logs", "--follow", "--lines", "10")
	cmd.Env = s.env()
	cmd.Stderr = os.Stderr
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start follow: %v", err)
	}

	f := &followProc{cmd: cmd, done: make(chan error, 1)}
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		sc := bufio.NewScanner(pipe)
		for sc.Scan() {
			f.mu.Lock()
			f.lines = append(f.lines, sc.Text())
			f.mu.Unlock()
		}
	}()
	go func() {
		<-scanned // Wait closes the pipe, so drain it first
		f.done <- cmd.Wait()
	}()
	return f
}

// waitFor blocks until a line containing want has been printed.
func (f *followProc) waitFor(t *testing.T, want, what string) {
	t.Helper()
	deadline := time.Now().Add(followTimeout)
	for time.Now().Before(deadline) {
		if f.seen(want) {
			return
		}
		select {
		case err := <-f.done:
			t.Fatalf("follow exited before %s arrived (%v); output so far:\n%s",
				what, err, strings.Join(f.snapshot(), "\n"))
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("timed out waiting for %s (%q); output so far:\n%s",
		what, want, strings.Join(f.snapshot(), "\n"))
}

// seen reports whether any printed line contains want.
func (f *followProc) seen(want string) bool {
	for _, ln := range f.snapshot() {
		if strings.Contains(ln, want) {
			return true
		}
	}
	return false
}

// snapshot copies the lines printed so far.
func (f *followProc) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lines...)
}

// count is how many lines the follow has printed.
func (f *followProc) count() int { return len(f.snapshot()) }

// interrupt sends SIGINT and reports the exit code the command ended with.
func (f *followProc) interrupt(timeout time.Duration) (int, error) {
	if err := f.cmd.Process.Signal(os.Interrupt); err != nil {
		return -1, fmt.Errorf("send SIGINT: %w", err)
	}
	select {
	case err := <-f.done:
		if err == nil {
			return 0, nil
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), nil
		}
		return -1, err
	case <-time.After(timeout):
		return -1, fmt.Errorf("still running %s after SIGINT", timeout)
	}
}

// kill ends the process on a failed test, so a hung follow cannot outlive the
// run. Harmless once it has already exited.
func (f *followProc) kill() {
	if f.cmd.Process != nil {
		_ = f.cmd.Process.Kill()
	}
}

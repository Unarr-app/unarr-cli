package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

var (
	// errRejectedKey is what the daemon reports when the agent's key is no
	// longer valid — the failure seen on a real box.
	errRejectedKey = errors.New("register: API error 401: Invalid API key")
	// errDiskFull stands for any cause the tray does not recognise.
	errDiskFull = errors.New("write /var/lib/unarr: no space left on device")
)

// daemonStartupFailure is the real output of `unarr start` on a box whose agent
// key had been revoked — the failure that used to reach the user as nothing at
// all. Kept verbatim so the parsing is tested against what the daemon actually
// prints, banner and startup chatter included.
const daemonStartupFailure = `
  unarr Daemon

2026/07/20 11:12:47 [transcode] ffmpeg version 8.0.1, HW=none
2026/07/20 11:12:47 Transport: HTTP sync → http://localhost:3032 (mirrors: 1)
2026/07/20 11:12:48 [stream] server listening on port 11818
2026/07/20 11:12:48 [funnel] cloudflared started, waiting for public URL...
Error: register: register: API error 401: Invalid API key
`

func TestFailureReason(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{
			// The explicit Error: line wins over the startup lines that follow
			// the banner, and the wrapper's repeated scope is collapsed.
			name:   "real daemon startup failure",
			output: daemonStartupFailure,
			want:   "register: API error 401: Invalid API key",
		},
		{
			name:   "falls back to the last line when nothing is marked Error",
			output: "starting up\nsomething went sideways\n",
			want:   "something went sideways",
		},
		{
			name:   "prefers the Error line over later chatter",
			output: "Error: the real cause\n2026/07/20 shutting down\n",
			want:   "the real cause",
		},
		{
			name:   "no output at all",
			output: "",
			want:   "",
		},
		{
			name:   "only blank lines",
			output: "\n\n   \n",
			want:   "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := failureReason(tc.output); got != tc.want {
				t.Errorf("failureReason() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDedupeScopes(t *testing.T) {
	tests := []struct{ in, want string }{
		{"register: register: API error 401", "register: API error 401"},
		{"a: a: a: boom", "a: boom"},
		{"register: API error 401", "register: API error 401"},
		{"plain message", "plain message"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := dedupeScopes(tc.in); got != tc.want {
			t.Errorf("dedupeScopes(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDescribeControlFailure(t *testing.T) {
	t.Run("a rejected key tells the user how to fix it", func(t *testing.T) {
		// The most common failure and the only one the user can act on, so it
		// must not be reported as a raw wrapped error.
		got := describeControlFailure("start", errRejectedKey)

		if !strings.Contains(got.title, "sign-in") {
			t.Errorf("title = %q, want it to mention sign-in", got.title)
		}
		if !strings.Contains(got.detail, "unarr login") {
			t.Errorf("detail = %q, want it to name the command that fixes it", got.detail)
		}
		if !got.failed() {
			t.Error("a described failure must report failed()")
		}
	})

	t.Run("anything else is reported verbatim", func(t *testing.T) {
		// A cause we did not anticipate must survive to the user rather than be
		// flattened into a generic message.
		got := describeControlFailure("start", errDiskFull)

		if !strings.Contains(got.detail, "no space left on device") {
			t.Errorf("detail = %q, want the underlying reason kept", got.detail)
		}
		if !strings.Contains(got.title, "start") {
			t.Errorf("title = %q, want it to name the action", got.title)
		}
	})

	t.Run("the zero value is not a failure", func(t *testing.T) {
		// renderDaemonStatus relies on this to tell "no failure" from one.
		if (controlFailure{}).failed() {
			t.Error("the zero controlFailure must not report failed()")
		}
	})
}

func TestIsAuthFailure(t *testing.T) {
	authy := []string{
		"register: API error 401: Invalid API key",
		"Invalid API key",
		"unauthorized",
		"the key was revoked",
	}
	for _, s := range authy {
		if !isAuthFailure(s) {
			t.Errorf("isAuthFailure(%q) = false, want true", s)
		}
	}
	notAuthy := []string{
		"write /var/lib: no space left on device",
		"connection refused",
		"",
	}
	for _, s := range notAuthy {
		if isAuthFailure(s) {
			t.Errorf("isAuthFailure(%q) = true, want false", s)
		}
	}
}

func TestCappedBufferStaysBounded(t *testing.T) {
	// A daemon that starts successfully writes to this for as long as it runs,
	// so the cap is what keeps a long-lived tray from growing without limit.
	var b cappedBuffer
	chunk := strings.Repeat("x", 4096)

	total := 0
	for range 100 {
		n, err := b.Write([]byte(chunk))
		if err != nil {
			t.Fatalf("Write returned %v; it must never fail the child process", err)
		}
		if n != len(chunk) {
			t.Fatalf("Write reported %d of %d bytes; a short write would break the copier", n, len(chunk))
		}
		total += len(chunk)
	}

	if got := len(b.String()); got != controlOutputCap {
		t.Errorf("buffered %d bytes after writing %d, want it capped at %d", got, total, controlOutputCap)
	}
}

func TestAwaitControl(t *testing.T) {
	t.Run("reports why a command died early", func(t *testing.T) {
		// The bug this whole path exists for: the spawn succeeds and the
		// process fails afterwards.
		cmd, out := startHelper(t, "fail")

		err := awaitControl(cmd, out)

		if err == nil {
			t.Fatal("awaitControl returned nil for a command that exited 1")
		}
		if !strings.Contains(err.Error(), "API error 401: Invalid API key") {
			t.Errorf("error = %q, want the daemon's own reason", err)
		}
	})

	t.Run("a clean exit is not a failure", func(t *testing.T) {
		// `stop` exits 0 promptly and must not be reported as broken.
		cmd, out := startHelper(t, "ok")

		if err := awaitControl(cmd, out); err != nil {
			t.Errorf("awaitControl = %v, want nil for a clean exit", err)
		}
	})

	t.Run("a daemon still up after the window is a success", func(t *testing.T) {
		// `start` keeps running when it works, so outliving the window is the
		// signal that it came up.
		swapControlWatch(t, 50*time.Millisecond)
		cmd, out := startHelper(t, "linger")
		t.Cleanup(func() { _ = cmd.Process.Kill() })

		start := time.Now()
		err := awaitControl(cmd, out)

		if err != nil {
			t.Errorf("awaitControl = %v, want nil for a process that outlived the window", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("awaitControl took %s; it must return once the window closes", elapsed)
		}
	})

	t.Run("reaps the child instead of leaving a zombie", func(t *testing.T) {
		// The old fire-and-forget Start() never waited, so every pause/resume
		// leaked a zombie. ProcessState is only set once Wait has reaped it.
		cmd, out := startHelper(t, "ok")

		_ = awaitControl(cmd, out)

		if cmd.ProcessState == nil {
			t.Fatal("the child was never reaped")
		}
	})
}

func swapControlWatch(t *testing.T, d time.Duration) {
	t.Helper()
	prev := controlWatch
	controlWatch = d
	t.Cleanup(func() { controlWatch = prev })
}

// startHelper spawns this test binary as a stand-in child process, so the
// behaviours awaitControl cares about are exercised against a real process
// without depending on a shell that Windows does not have.
func startHelper(t *testing.T, mode string) (*exec.Cmd, *cappedBuffer) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestControlHelperProcess")
	cmd.Env = append(os.Environ(), "GO_CONTROL_HELPER=1", "GO_CONTROL_HELPER_MODE="+mode)
	out := &cappedBuffer{}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatalf("could not start the helper process: %v", err)
	}
	return cmd, out
}

// TestControlHelperProcess is not a test: it is the child startHelper spawns.
func TestControlHelperProcess(t *testing.T) {
	if os.Getenv("GO_CONTROL_HELPER") != "1" {
		t.Skip("helper process; only runs when spawned by startHelper")
	}
	switch os.Getenv("GO_CONTROL_HELPER_MODE") {
	case "fail":
		os.Stderr.WriteString(daemonStartupFailure)
		os.Exit(1)
	case "linger":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	default:
		os.Exit(0)
	}
}

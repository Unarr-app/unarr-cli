package main

// Supervision for daemon control commands (start/stop/restart).
//
// The tray used to fire these and look only at the spawn error, so a daemon
// that started fine and then exited — the common case, e.g. a rejected API key
// — reported success and the menu silently fell back to "stopped". The user
// pressed Resume and nothing happened, with no error anywhere: the only report
// went to stderr, which a GUI process has no terminal for.
//
// So a control command is now watched for a bounded window: if it exits with a
// failure inside it, the reason is captured and turned into something
// actionable. Past the window the daemon is up and the watch stops mattering —
// but the wait keeps running so the child is always reaped.

import (
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/winproc"
)

// controlWatch is how long a daemon control is watched for an early exit. Long
// enough for the daemon's startup (transcode probe, transport, registration —
// ~2s locally) to fail, short enough that a wedged binary is not watched
// forever. Past it the daemon is considered up.
var controlWatch = 10 * time.Second

// signInWatch is the window for `login --browser`, which is not a daemon
// control at all: it opens a browser and waits on a local callback for up to a
// minute. Watching it for only controlWatch would call an abandoned sign-in a
// success and report nothing when it finally timed out.
var signInWatch = 75 * time.Second

// signInAction is the action name for the browser sign-in the menu offers in
// place of controls that cannot work with a rejected key.
const signInAction = "sign-in"

// watchFor is how long the given action is watched before it counts as started.
func watchFor(action string) time.Duration {
	if action == signInAction {
		return signInWatch
	}
	return controlWatch
}

// controlOutputCap bounds what is kept from a control command's output. Only
// the failure reason is ever needed, and a daemon that starts successfully
// writes to this for as long as it lives — so it must never grow unbounded.
const controlOutputCap = 8 << 10

// controlFailure is a failed control command in the terms the user sees.
type controlFailure struct {
	// title replaces the status row, so it must read as a state.
	title string
	// detail is the dialog body and the row's tooltip: what went wrong and what
	// to do about it.
	detail string
	// authRequired: the agent's credential was rejected, so no control will
	// work until the user signs in again. The menu collapses to that one action
	// rather than offering buttons that are guaranteed to fail.
	authRequired bool
}

// failed reports whether this is a real failure rather than the zero value
// stored to clear one.
func (c controlFailure) failed() bool { return c.title != "" }

// cappedBuffer keeps at most cap bytes of what is written to it and silently
// drops the rest. Writes never fail and never block: it is wired to a child
// process's stdout/stderr, which must not be affected by the tray's interest
// in it. Safe for concurrent use — the exec package copies on its own goroutine.
type cappedBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := controlOutputCap - len(c.buf); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		c.buf = append(c.buf, p[:room]...)
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.buf)
}

// startUnarr spawns `unarr <args…>` with its output captured. The child is not
// tied to the tray's lifetime: the tray only waits on it to learn whether it
// died early and to reap it.
func startUnarr(args ...string) (*exec.Cmd, *cappedBuffer, error) {
	cmd := exec.Command(unarrBin(), args...)
	winproc.HideWindow(cmd)
	out := &cappedBuffer{}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	return cmd, out, nil
}

// awaitControl reports why a control command failed, or nil if it is still
// running after controlWatch (started) or exited cleanly (stop/restart).
//
// The wait runs on its own goroutine and is never abandoned, so the child is
// reaped even when the watch window expires first — the previous fire-and-
// forget Start() left a zombie behind on every pause/resume.
// late, when set, receives a failure that arrives AFTER the watch window. The
// window is a UI deadline — how long the tray waits before assuming the command
// worked — not a limit on how long a command can take to fail. Registration
// retries transient errors for over a minute before giving up, so the failure
// the user most needs to hear about was the one guaranteed to miss the window:
// its exit code and the whole captured output were collected and then thrown
// away, leaving "Agent: crashed" with an empty tooltip.
func awaitControl(cmd *exec.Cmd, out *cappedBuffer, within time.Duration, late func(error)) error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		return controlError(err, out)
	case <-time.After(within):
		// Still running, so the tray stops waiting and reports nothing — but it
		// keeps listening, because a command that dies a minute from now failed
		// just as much as one that died in five seconds.
		if late != nil {
			go func() {
				if err := controlError(<-done, out); err != nil {
					late(err)
				}
			}()
		}
		return nil
	}
}

// controlError turns a finished command into the reason it failed, or nil if it
// succeeded. Read only after Wait has returned: the output copiers are finished,
// so this sees everything the child wrote.
func controlError(err error, out *cappedBuffer) error {
	if err == nil {
		return nil
	}
	if reason := failureReason(out.String()); reason != "" {
		return fmt.Errorf("%s", reason)
	}
	return err
}

// failureReason picks the line of a failed command's output that explains it.
// The daemon logs a banner and a dozen startup lines before the failure, so the
// explicit "Error:" line is preferred over merely the last one; its prefix and
// the wrapper's repeated scopes ("register: register: …") are stripped so the
// text reads as a sentence.
//
// Lines are split on carriage returns as well as newlines, and ANSI escapes are
// stripped: progress output like the sign-in countdown redraws one line with
// \r and clears it with an escape, so splitting on \n alone would treat a
// minute of "Waiting… 60s/59s/58s" and the error that follows it as a single
// line — and show the user all of it.
func failureReason(output string) string {
	errLine, lastLine := "", ""
	for line := range strings.FieldsFuncSeq(stripANSI(output), func(r rune) bool {
		return r == '\n' || r == '\r'
	}) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if after, found := strings.CutPrefix(line, "Error:"); found {
			errLine = strings.TrimSpace(after)
		}
		lastLine = line
	}
	if errLine != "" {
		return dedupeScopes(errLine)
	}
	return dedupeScopes(lastLine)
}

// stripANSI removes escape sequences from command output. Progress rendering
// emits them to clear and repaint a line; left in, they reach the user as
// mojibake like "[K" in the middle of an error message.
func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == 0x1b { // ESC: skip to the end of the sequence
			i++
			if i < len(s) && s[i] == '[' {
				i++
				for i < len(s) && (s[i] < '@' || s[i] > '~') {
					i++
				}
			}
			if i < len(s) {
				i++ // the final byte of the sequence
			}
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// dedupeScopes collapses a repeated leading scope from wrapped errors, so
// "register: register: API error 401" reads as "register: API error 401".
func dedupeScopes(msg string) string {
	for {
		scope, rest, found := strings.Cut(msg, ": ")
		if !found {
			return msg
		}
		if !strings.HasPrefix(rest, scope+": ") {
			return msg
		}
		msg = rest
	}
}

// describeControlFailure turns a failed control into what the user is shown.
// The rejected-key case gets its own wording because it is both the most common
// failure and the one the user can actually fix; anything else is reported
// verbatim rather than flattened into a generic message.
func describeControlFailure(action string, err error) controlFailure {
	reason := err.Error()
	// An agent that predates `login --browser` rejects the flag outright. The
	// two binaries ship in the same release, so this only happens on a mixed
	// install — and "unknown flag: --browser" tells the user nothing they can
	// act on, while "update the agent" does.
	if isMissingBrowserFlag(reason) {
		return controlFailure{
			title:  "Agent: update needed",
			detail: "This unarr agent is too old to sign in from the menu. Update it (re-run the unarr installer), then try Sign in again.",
		}
	}
	if isAuthFailure(reason) {
		// No terminal in the message: the tray exists precisely so the user
		// never needs one, and "Sign in…" in this menu does it for them.
		return controlFailure{
			title:        "Agent: sign-in required",
			detail:       "unarr rejected this agent's key. Choose “Sign in…” in the unarr menu to reconnect this machine.",
			authRequired: true,
		}
	}
	return controlFailure{
		title:  "Agent: " + action + " failed",
		detail: strings.ToUpper(action[:1]) + action[1:] + " failed: " + reason,
	}
}

// isMissingBrowserFlag recognises an agent too old to know `login --browser`.
func isMissingBrowserFlag(reason string) bool {
	lower := strings.ToLower(reason)
	return strings.Contains(lower, "unknown flag") && strings.Contains(lower, "browser")
}

// isAuthFailure recognises a rejected or missing agent key, whatever layer
// reported it.
func isAuthFailure(reason string) bool {
	lower := strings.ToLower(reason)
	for _, marker := range []string{"invalid api key", "api error 401", "401", "unauthorized", "revoked"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

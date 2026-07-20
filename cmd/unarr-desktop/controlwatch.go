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
func awaitControl(cmd *exec.Cmd, out *cappedBuffer, within time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err == nil {
			return nil
		}
		// Read only after Wait has returned: the output copiers are finished,
		// so this sees everything the child wrote.
		if reason := failureReason(out.String()); reason != "" {
			return fmt.Errorf("%s", reason)
		}
		return err
	case <-time.After(within):
		return nil
	}
}

// failureReason picks the line of a failed command's output that explains it.
// The daemon logs a banner and a dozen startup lines before the failure, so the
// explicit "Error:" line is preferred over merely the last one; its prefix and
// the wrapper's repeated scopes ("register: register: …") are stripped so the
// text reads as a sentence.
func failureReason(output string) string {
	errLine, lastLine := "", ""
	for line := range strings.SplitSeq(output, "\n") {
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

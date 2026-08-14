package cmd

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// A streaming session that ends without EITHER a ready flip or a reported error
// leaves streaming_session with ready_at and error_code both NULL server-side.
// The player can then only render the generic "your agent is taking too long",
// which is the same screen someone with no agent at all sees, and no dashboard
// can tell the two apart. Prod (jul-ago 2026): 287 sessions died exactly like
// that — 65 at watchSessionReady's deadline, the rest torn down before it.
//
// watchSessionReady isn't injectable without a wider refactor of the daemon
// wiring, so these lock the invariant at the source level: every pre-ready exit
// from that loop must go through failSession, never a bare return.
func daemonSource(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("daemon.go")
	if err != nil {
		t.Fatalf("read daemon.go: %v", err)
	}
	return string(src)
}

// watchSessionReady's body, so assertions can't be satisfied by an unrelated
// part of this 4k-line file.
func watchSessionReadyBody(t *testing.T) string {
	t.Helper()
	src := daemonSource(t)
	start := strings.Index(src, "func watchSessionReady(")
	if start < 0 {
		t.Fatal("watchSessionReady not found — was it renamed?")
	}
	rest := src[start:]
	// Ends at the next top-level func declaration.
	if end := regexp.MustCompile(`\n\nfunc `).FindStringIndex(rest); end != nil {
		return rest[:end[0]]
	}
	return rest
}

func TestStartTimeoutIsReported(t *testing.T) {
	body := watchSessionReadyBody(t)
	if !strings.Contains(body, "sessErrStartTimeout") {
		t.Error("the seg-0 deadline must report sessErrStartTimeout, not just log locally: " +
			"a silent return leaves the session invisible (ready_at + error_code both NULL)")
	}
	// The old bug in one line: log and return, nothing reported.
	if strings.Contains(body, `log.Printf("[hls %s] mark-ready: timeout waiting for seg-0"`) {
		t.Error("the timeout path still only logs; it must call failSession")
	}
}

func TestClosedBeforeReadyIsReported(t *testing.T) {
	body := watchSessionReadyBody(t)
	idx := strings.Index(body, "if hsess.IsClosed() {")
	if idx < 0 {
		t.Fatal("IsClosed guard not found in watchSessionReady")
	}
	// The guard's own block, up to its closing return.
	block := body[idx:]
	if end := strings.Index(block, "\t\t}\n"); end > 0 {
		block = block[:end]
	}
	if !strings.Contains(block, "failSession") {
		t.Error("a session torn down BEFORE serving its first segment must be reported: " +
			"with MaxStreamSessions=1 an impatient retry displaces the previous one " +
			"through this exact path, and it used to vanish silently")
	}
	// It must stay conditional: after the ready flip a teardown is the normal
	// end of playback, and reporting it would turn a success into a failure.
	if !strings.Contains(block, "!readyPosted") {
		t.Error("the report must be gated on !readyPosted, or normal end-of-playback " +
			"teardowns get reported as failures")
	}
}

func TestStartTimeoutConstantMatchesTheWebEnum(t *testing.T) {
	// The web validates this string with a zod enum and 400s anything else,
	// which is how `transcode_failed` was silently dropped for weeks: the Go
	// const shipped, SESSION_ERROR_CODES never learned about it, and every
	// report was rejected. Keep the literal in step with
	// src/lib/stream/session-error-codes.ts.
	if sessErrStartTimeout != "start_timeout" {
		t.Errorf("sessErrStartTimeout = %q, want %q (must match the web enum)",
			sessErrStartTimeout, "start_timeout")
	}
}

func TestStartTimeoutDeadlineIsNamed(t *testing.T) {
	body := watchSessionReadyBody(t)
	if strings.Contains(body, "time.Now().Add(60 * time.Second)") {
		t.Error("the deadline must use the startTimeout const so the value and the " +
			"message reporting it cannot drift apart")
	}
}

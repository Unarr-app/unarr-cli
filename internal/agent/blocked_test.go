package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// withTempStateDir points the state (and therefore the blocked) file at a temp
// dir so tests never touch the developer's real agent data.
func withTempStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := stateFilePathFn
	stateFilePathFn = func() string { return filepath.Join(dir, "daemon.state.json") }
	t.Cleanup(func() { stateFilePathFn = prev })
	return dir
}

func TestClassifySeparatesUserActionFromRetry(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantReason BlockReason
		terminal   bool
	}{
		{
			// The failure that started this: a rejected key looped the daemon
			// under systemd forever and reported 25 crashes, because it was
			// neither "transient" nor "revoked" and so had no home at all.
			name:       "rejected credential",
			err:        &HTTPError{StatusCode: 401, Message: "Invalid API key"},
			wantReason: BlockSignIn, terminal: true,
		},
		{
			name:       "agent deleted from the dashboard",
			err:        &HTTPError{StatusCode: 410, Message: "agent_revoked"},
			wantReason: BlockRevoked, terminal: true,
		},
		{
			name:       "key belongs to another machine",
			err:        &HTTPError{StatusCode: 403, Message: "agent_key_mismatch"},
			wantReason: BlockRevoked, terminal: true,
		},
		{
			name:       "plan is out of machine slots",
			err:        &HTTPError{StatusCode: 403, Message: "agent_limit_reached"},
			wantReason: BlockPlan, terminal: true,
		},
		{
			name:       "another machine claims this identity",
			err:        &HTTPError{StatusCode: 409, Message: "agent_hash_taken"},
			wantReason: BlockConflict, terminal: true,
		},
		{
			// Everything below must stay OUT: declaring an unknown or temporary
			// error terminal would park a daemon that a retry would have fixed.
			name: "server error", err: &HTTPError{StatusCode: 500, Message: "boom"},
		},
		{name: "rate limited", err: &HTTPError{StatusCode: 429, Message: "rate_limit"}},
		{name: "not found", err: &HTTPError{StatusCode: 404, Message: "nope"}},
		{name: "plain network error", err: errors.New("connection refused")},
		{name: "no error", err: nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, terminal := Classify(tc.err)
			if terminal != tc.terminal {
				t.Fatalf("Classify(%v) terminal = %v, want %v", tc.err, terminal, tc.terminal)
			}
			if !terminal {
				return
			}
			if b.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", b.Reason, tc.wantReason)
			}
			if b.Message == "" {
				t.Error("no message: a blocked user with nothing to read is the bug this fixes")
			}
			if b.Remedy == "" {
				t.Error("no remedy: naming the problem without the next step is a dead end")
			}
			if strings.Contains(b.Message, "_") && !strings.Contains(b.Message, " ") {
				t.Errorf("message %q is a machine code, not something to show a user", b.Message)
			}
		})
	}
}

func TestClassifyPrefersTheServersWording(t *testing.T) {
	// The server knows specifics the client cannot — which plan, how many
	// machines — and can improve its wording without a new binary reaching
	// every user. That message was being thrown away by the client.
	const detail = "Agent limit reached for your plan (2 active agents)."
	b, ok := Classify(&HTTPError{StatusCode: 403, Message: "agent_limit_reached", Detail: detail})
	if !ok {
		t.Fatal("not classified as terminal")
	}
	if b.Message != detail {
		t.Errorf("message = %q, want the server's sentence %q", b.Message, detail)
	}
}

func TestClassifyFallsBackWhenTheServerSendsOnlyACode(t *testing.T) {
	// Older servers send no message field. Showing "agent_limit_reached" to a
	// user is not an explanation.
	b, ok := Classify(&HTTPError{StatusCode: 403, Message: "agent_limit_reached"})
	if !ok {
		t.Fatal("not classified as terminal")
	}
	if b.Message == "agent_limit_reached" {
		t.Error("fell through to the raw error code instead of a readable sentence")
	}
}

func TestBlockedRoundTrip(t *testing.T) {
	withTempStateDir(t)

	if got := ReadBlocked(); got != nil {
		t.Fatalf("ReadBlocked() = %+v on a clean install, want nil", got)
	}

	want := &Blocked{Reason: BlockSignIn, Message: "m", Remedy: "r", Status: 401}
	WriteBlocked(want)

	got := ReadBlocked()
	if got == nil {
		t.Fatal("ReadBlocked() = nil after WriteBlocked")
	}
	if got.Reason != want.Reason || got.Message != want.Message || got.Remedy != want.Remedy {
		t.Errorf("read back %+v, want %+v", got, want)
	}
	if got.At.IsZero() {
		t.Error("no timestamp: a support report cannot tell a live block from a stale one")
	}

	ClearBlocked()
	if got := ReadBlocked(); got != nil {
		t.Error("ReadBlocked() still returns a block after ClearBlocked — a fixed problem must stop being reported")
	}
}

func TestBlockedFileIsNotWorldReadable(t *testing.T) {
	// The record can name the account's plan and limits; a co-tenant on a
	// shared host has no business reading it. Same 0600 as the config.
	withTempStateDir(t)
	WriteBlocked(&Blocked{Reason: BlockPlan, Message: "m", Remedy: "r"})

	fi, err := os.Stat(BlockedFilePath())
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

func TestReadBlockedIgnoresGarbage(t *testing.T) {
	// A truncated or hand-edited file must degrade to "not blocked" rather than
	// wedging the tray on a block nobody can clear.
	dir := withTempStateDir(t)
	for _, content := range []string{"", "{", "not json at all", `{"reason":""}`} {
		if err := os.WriteFile(filepath.Join(dir, "blocked.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := ReadBlocked(); got != nil {
			t.Errorf("ReadBlocked() = %+v for %q, want nil", got, content)
		}
	}
}

// registerServer serves /api/internal/agent/register, rejecting until the
// caller presents wantKey — the shape of a user signing in mid-block.
func registerServer(t *testing.T, wantKey string, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+wantKey {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(ErrorResponse{Error: "Invalid API key"})
			return
		}
		json.NewEncoder(w).Encode(RegisterResponse{})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWaitOutBlockRecoversWhenTheUserSignsIn(t *testing.T) {
	// The whole point of parking instead of exiting: signing in from the tray
	// rewrites the credential, and the daemon must pick it up on its own.
	// Otherwise a successful sign-in looks like it did nothing — the dead end
	// this path exists to remove.
	withTempStateDir(t)
	prev := blockedRetry
	blockedRetry = 5 * time.Millisecond
	t.Cleanup(func() { blockedRetry = prev })

	var calls atomic.Int32
	srv := registerServer(t, "good-key", &calls)

	d := &Daemon{client: NewClient(srv.URL, "stale-key", "test")}
	var notified atomic.Int32
	d.OnBlocked = func(*Blocked) { notified.Add(1) }
	// The user signs in on the third attempt.
	d.ReloadCredential = func() {
		if calls.Load() >= 2 {
			d.client.SetAPIKey("good-key")
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := d.waitOutBlock(ctx, &Blocked{Reason: BlockSignIn, Message: "m", Remedy: "r"}, RegisterRequest{})
	if err != nil {
		t.Fatalf("waitOutBlock() = %v, want recovery", err)
	}
	if resp == nil {
		t.Fatal("recovered with a nil response")
	}
	if got := ReadBlocked(); got != nil {
		t.Errorf("block %+v survived recovery — the user would still be told to sign in", got)
	}
	if n := notified.Load(); n != 1 {
		t.Errorf("notified %d times, want exactly 1: a blocked user does not need the same popup every minute", n)
	}
}

func TestWaitOutBlockRecordsTheBlockForTheTray(t *testing.T) {
	// The tray has no other way to learn why the agent is idle: it never sees
	// the daemon's exit code or its stderr.
	withTempStateDir(t)
	prev := blockedRetry
	blockedRetry = time.Hour // never retries within the test
	t.Cleanup(func() { blockedRetry = prev })

	d := &Daemon{client: NewClient("http://127.0.0.1:1", "k", "test")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // blocked, then shut down

	_, err := d.waitOutBlock(ctx, &Blocked{Reason: BlockPlan, Message: "m", Remedy: "r"}, RegisterRequest{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	got := ReadBlocked()
	if got == nil || got.Reason != BlockPlan {
		t.Fatalf("recorded block = %+v, want a plan block on disk", got)
	}
}

func TestWaitOutBlockStopsOnShutdown(t *testing.T) {
	// A parked daemon must still answer SIGTERM promptly; a user stopping the
	// service should not wait out the retry interval.
	withTempStateDir(t)
	prev := blockedRetry
	blockedRetry = 50 * time.Millisecond
	t.Cleanup(func() { blockedRetry = prev })

	var calls atomic.Int32
	srv := registerServer(t, "never-matches", &calls)
	d := &Daemon{client: NewClient(srv.URL, "bad", "test")}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(120 * time.Millisecond)
		cancel()
	}()

	done := make(chan struct{})
	go func() {
		d.waitOutBlock(ctx, &Blocked{Reason: BlockSignIn, Message: "m", Remedy: "r"}, RegisterRequest{})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("waitOutBlock did not return after the context was cancelled")
	}
}

package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// These cover the defects a review found in the first cut of the parking work.
// Each one shipped green, so each gets a test that would have caught it.

func TestRegisterBestEffortNeverParks(t *testing.T) {
	// The worst of them: the ACME pre-cert bootstrap calls Register long before
	// the recovery callbacks are wired and before the signal handlers exist. It
	// is explicitly best-effort — it logs and moves on. Parking there stranded
	// the process somewhere it could neither be stopped nor ever recover.
	withTempStateDir(t)
	prev := blockedRetry
	blockedRetry = time.Hour // parking would hang this test
	prevBackoff := registerBackoff
	registerBackoff = time.Millisecond // the ambiguous-401 retries, without the wait
	t.Cleanup(func() { blockedRetry = prev; registerBackoff = prevBackoff })

	var calls atomic.Int32
	srv := registerServer(t, "never-matches", &calls)
	d := &Daemon{client: NewClient(srv.URL, "bad", "test")}

	done := make(chan error, 1)
	go func() { done <- d.RegisterBestEffort(context.Background()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("RegisterBestEffort() = nil on a rejected credential")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RegisterBestEffort parked — a best-effort call must always return")
	}

	// It still records the block, so the tray can explain a daemon that goes on
	// to fail the same way in Run.
	if got := ReadBlocked(); got == nil {
		t.Error("no block recorded — the failure would be invisible to the tray")
	}
}

func TestParkedDaemonIsVisibleToTheRestOfTheCLI(t *testing.T) {
	// A parked daemon holds the lock file. Without publishing state it is
	// invisible to `unarr stop` (which reads the PID from there) while
	// `unarr start` still refuses to run — telling the user at once that no
	// daemon is running and that one already is.
	withTempStateDir(t)
	prev := blockedRetry
	blockedRetry = time.Hour
	t.Cleanup(func() { blockedRetry = prev })

	d := &Daemon{client: NewClient("http://127.0.0.1:1", "k", "test")}
	d.cfg.AgentID = "agent-1"

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.waitOutBlock(ctx, &Blocked{Reason: BlockSignIn, Message: "m", Remedy: "r"}, RegisterRequest{})

	st := ReadState()
	if st == nil {
		t.Fatal("a parked daemon published no state: `unarr stop` cannot find it")
	}
	if st.PID == 0 {
		t.Error("state carries no PID, so there is nothing to signal")
	}
	if st.Status == "running" {
		t.Error(`status is "running": a parked daemon must not read as a working one`)
	}
}

func TestParkedDaemonAdoptsANewAgentIdentity(t *testing.T) {
	// After a revocation the credential AND the agent id are wiped, and signing
	// in mints both. Re-sending the tombstoned id would be rejected forever no
	// matter how good the new key — the recovery would silently never happen.
	withTempStateDir(t)
	prev := blockedRetry
	blockedRetry = 5 * time.Millisecond
	t.Cleanup(func() { blockedRetry = prev })

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req RegisterRequest
		json.NewDecoder(r.Body).Decode(&req)
		// The server tombstoned "old-agent": only the fresh identity is served.
		if req.AgentID != "new-agent" {
			w.WriteHeader(http.StatusGone)
			json.NewEncoder(w).Encode(ErrorResponse{Error: "agent_revoked"})
			return
		}
		json.NewEncoder(w).Encode(RegisterResponse{})
	}))
	defer srv.Close()

	d := &Daemon{client: NewClient(srv.URL, "k", "test")}
	d.cfg.AgentID = "old-agent"
	d.OnCredentialRejected = func() {}
	d.ReloadCredential = func() { d.SetAgentID("new-agent") } // the sign-in

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := d.waitOutBlock(ctx, &Blocked{Reason: BlockRevoked, Message: "m", Remedy: "r"},
		RegisterRequest{AgentID: "old-agent"}); err != nil {
		t.Fatalf("waitOutBlock() = %v — the daemon never adopted the new identity", err)
	}
}

func TestAnAmbiguousRejectionIsRetriedBeforeItIsBelieved(t *testing.T) {
	// A bare 401 can be a deploy blip. Parking on the first one would, during
	// any auth wobble, pop a critical notification on every machine in the fleet
	// telling perfectly fine users to sign in again.
	withTempStateDir(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Rejects once, like a blip, then recovers.
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(ErrorResponse{Error: "Invalid API key"})
			return
		}
		json.NewEncoder(w).Encode(RegisterResponse{})
	}))
	defer srv.Close()

	prevBackoff := registerBackoff
	registerBackoff = time.Millisecond
	t.Cleanup(func() { registerBackoff = prevBackoff })

	d := &Daemon{client: NewClient(srv.URL, "k", "test")}
	var notified atomic.Int32
	d.OnBlocked = func(*Blocked) { notified.Add(1) }

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := d.Register(ctx); err != nil {
		t.Fatalf("Register() = %v, want recovery on the retry", err)
	}
	if n := notified.Load(); n != 0 {
		t.Errorf("shouted at the user %d times over a blip that cleared on retry", n)
	}
	if got := ReadBlocked(); got != nil {
		t.Errorf("left a block behind after recovering: %+v", got)
	}
}

func TestAnExplicitRejectionParksImmediately(t *testing.T) {
	// The counterpart: revoked / plan-limit / conflict are unambiguous. Retrying
	// them is just latency between the user and the answer.
	for _, tc := range []struct {
		name   string
		reason BlockReason
	}{
		{"revoked", BlockRevoked},
		{"plan limit", BlockPlan},
		{"identity conflict", BlockConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if ambiguousEnoughToRetry(&Blocked{Reason: tc.reason}, 0) {
				t.Errorf("%s is retried before parking, but retrying cannot change the answer", tc.reason)
			}
		})
	}
	if !ambiguousEnoughToRetry(&Blocked{Reason: BlockSignIn}, 0) {
		t.Error("a bare 401 parks on the first attempt")
	}
	if ambiguousEnoughToRetry(&Blocked{Reason: BlockSignIn}, ambiguousRetries) {
		t.Error("a 401 is retried forever instead of eventually being believed")
	}
}

func TestSyncRecordsARejectionOnceNotEveryTick(t *testing.T) {
	// The sync loop runs every few seconds. Rewriting the same record each pass
	// is thousands of pointless disk writes a day, and notifying each pass would
	// be worse.
	withTempStateDir(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
		json.NewEncoder(w).Encode(ErrorResponse{Error: "agent_revoked"})
	}))
	defer srv.Close()

	sc := NewSyncClient(NewClient(srv.URL, "k", "test"), DaemonConfig{}, NewLocalState())
	var notified atomic.Int32
	sc.OnBlocked = func(*Blocked) { notified.Add(1) }

	for range 5 {
		sc.doSync(context.Background())
	}

	if n := notified.Load(); n != 1 {
		t.Errorf("notified %d times over five ticks of one failure, want 1", n)
	}
	if got := ReadBlocked(); got == nil {
		t.Error("a rejected sync recorded nothing: the tray would show a healthy agent")
	}
}

func TestSyncClearsTheBlockWhenItRecovers(t *testing.T) {
	withTempStateDir(t)

	var fail atomic.Bool
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusGone)
			json.NewEncoder(w).Encode(ErrorResponse{Error: "agent_revoked"})
			return
		}
		json.NewEncoder(w).Encode(SyncResponse{})
	}))
	defer srv.Close()

	sc := NewSyncClient(NewClient(srv.URL, "k", "test"), DaemonConfig{}, NewLocalState())
	sc.doSync(context.Background())
	if ReadBlocked() == nil {
		t.Fatal("no block recorded while failing")
	}

	fail.Store(false)
	sc.doSync(context.Background())

	if got := ReadBlocked(); got != nil {
		t.Errorf("block %+v survived recovery — the user is still told to act on a fixed problem", got)
	}
}

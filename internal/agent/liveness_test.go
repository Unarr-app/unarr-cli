package agent

import (
	"testing"
	"time"
)

// TestLastAliveAtPrefersTheLivenessStamp covers the distinction the two fields
// exist for: LastHeartbeat says when the SERVER last answered, LastAlive says
// when the DAEMON last ran. On a box with no network the first freezes and the
// second keeps moving, and reading the wrong one made `unarr status` report a
// downloading agent as not running.
func TestLastAliveAtPrefersTheLivenessStamp(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name  string
		state *DaemonState
		want  time.Time
		why   string
	}{
		{
			name:  "offline daemon: alive is fresh, heartbeat is ancient",
			state: &DaemonState{LastHeartbeat: now.Add(-9 * time.Hour), LastAlive: now.Add(-3 * time.Second)},
			want:  now.Add(-3 * time.Second),
			why:   "the daemon is running; only the server is unreachable",
		},
		{
			name:  "healthy daemon: both fresh",
			state: &DaemonState{LastHeartbeat: now.Add(-2 * time.Second), LastAlive: now.Add(-1 * time.Second)},
			want:  now.Add(-1 * time.Second),
			why:   "the newer of the two is the best evidence",
		},
		{
			name:  "state file written by an older daemon that had no LastAlive",
			state: &DaemonState{LastHeartbeat: now.Add(-30 * time.Second)},
			want:  now.Add(-30 * time.Second),
			why:   "a missing LastAlive must fall back, not read as the zero time",
		},
		{
			name:  "nothing recorded at all",
			state: &DaemonState{},
			want:  time.Time{},
			why:   "callers check IsZero to mean 'undatable'",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LastAliveAt(tc.state); !got.Equal(tc.want) {
				t.Fatalf("LastAliveAt() = %v, want %v — %s", got, tc.want, tc.why)
			}
		})
	}
	if got := LastAliveAt(nil); !got.IsZero() {
		t.Fatalf("LastAliveAt(nil) = %v, want the zero time", got)
	}
}

// TestSyncAttemptFiresOnFailure is the guarantee the whole fix rests on: the
// liveness callback runs even when the sync errors out. Wiring it to
// OnSyncSuccess instead — which is what the daemon effectively had — is exactly
// the bug, so this pins the difference rather than the plumbing.
func TestSyncAttemptFiresOnFailure(t *testing.T) {
	var attempts, successes int
	sc := &SyncClient{
		OnSyncAttempt: func() { attempts++ },
		OnSyncSuccess: func() { successes++ },
	}

	// A client with no HTTP client and no config fails inside doSync. Whatever
	// the failure mode, the attempt callback must still have fired.
	func() {
		defer func() { _ = recover() }() // a nil client may panic; the defer still runs
		sc.doSync(t.Context())
	}()

	if attempts != 1 {
		t.Fatalf("OnSyncAttempt fired %d times on a failed sync, want 1 — an offline "+
			"daemon would stop refreshing its state file and be read as dead", attempts)
	}
	if successes != 0 {
		t.Fatalf("OnSyncSuccess fired %d times on a failed sync, want 0", successes)
	}
}

// TestStateFromPreviousBootUsesLiveness: the reboot check dates a state file by
// the same liveness stamp, so an offline daemon is not mistaken for a relic of
// the previous boot either.
func TestStateFromPreviousBootUsesLiveness(t *testing.T) {
	now := time.Now()
	fakeBoot(t, now.Add(-30*time.Minute), true)

	offlineButAlive := &DaemonState{
		Status:        "running",
		PID:           1,
		StartedAt:     now.Add(-8 * time.Hour), // booted before... but see LastAlive
		LastHeartbeat: now.Add(-8 * time.Hour), // sync has been failing all day
		LastAlive:     now.Add(-5 * time.Second),
	}
	if StateFromPreviousBoot(offlineButAlive) {
		t.Fatal("a daemon that ran its sync loop five seconds ago is not from the previous boot")
	}
}

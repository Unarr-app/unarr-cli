package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

func TestDaemonHealth(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	up := func(*agent.DaemonState) bool { return true }
	down := func(*agent.DaemonState) bool { return false }

	cases := []struct {
		name    string
		state   *agent.DaemonState
		alive   func(*agent.DaemonState) bool
		wantErr bool
		want    string
	}{
		{"no daemon installed", nil, down, false, "not running"},
		{"just started, no sync yet", &agent.DaemonState{PID: 7, StartedAt: now.Add(-30 * time.Second)}, up, false, "running"},
		{"inside the grace, no sync yet", &agent.DaemonState{PID: 7, StartedAt: now.Add(-daemonFirstSyncGrace + time.Second)}, up, false, "running"},
		{"2026-09-23: up a day, never synced", &agent.DaemonState{PID: 7, StartedAt: now.Add(-24 * time.Hour)}, up, true, "never ran"},
		{"syncing normally", &agent.DaemonState{PID: 7, StartedAt: now.Add(-24 * time.Hour), LastAlive: now.Add(-5 * time.Second)}, up, false, "running"},
		{"offline but alive (heartbeat old, alive fresh)", &agent.DaemonState{PID: 7, StartedAt: now.Add(-24 * time.Hour), LastHeartbeat: now.Add(-3 * time.Hour), LastAlive: now.Add(-5 * time.Second)}, up, false, "running"},
		{"sync loop hung later", &agent.DaemonState{PID: 7, StartedAt: now.Add(-24 * time.Hour), LastAlive: now.Add(-10 * time.Minute)}, down, true, "stopped syncing"},
		{"process gone, never synced", &agent.DaemonState{PID: 7, StartedAt: now.Add(-time.Minute)}, down, true, "process is gone"},
		{"pre-LastAlive file, fresh heartbeat", &agent.DaemonState{PID: 7, StartedAt: now.Add(-time.Hour), LastHeartbeat: now.Add(-5 * time.Second)}, up, false, "running"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := daemonHealth(tc.state, now, tc.alive)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v (msg %q)", err, tc.wantErr, msg)
			}
			if !strings.Contains(msg, tc.want) {
				t.Errorf("msg = %q, want it to mention %q", msg, tc.want)
			}
		})
	}
}

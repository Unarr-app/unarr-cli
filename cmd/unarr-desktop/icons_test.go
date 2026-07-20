package main

import (
	"bytes"
	"image/png"
	"testing"
)

func TestDisplayState(t *testing.T) {
	tests := []struct {
		name   string
		status agentStatus
		paused bool
		failed bool
		want   trayState
	}{
		{"running", agentStatus{running: true}, false, false, stateRunning},
		{"running wins over stale paused marker", agentStatus{running: true}, true, false, stateRunning},
		{"downloading when running with active tasks", agentStatus{running: true, tasks: 3}, false, false, stateDownloading},
		{"tasks ignored when not running", agentStatus{tasks: 3}, false, false, stateStopped},
		{"crashed", agentStatus{crashed: true, pid: 9}, false, false, stateCrashed},
		{"crashed wins over paused", agentStatus{crashed: true, pid: 9}, true, false, stateCrashed},
		{"paused", agentStatus{}, true, false, statePaused},
		{"stopped", agentStatus{}, false, false, stateStopped},
		// A failed control must reach the icon: "stopped" is the part the user
		// already knows, "why" is the part they are missing.
		{"failed control", agentStatus{}, false, true, stateFailed},
		{"failed outranks paused", agentStatus{}, true, true, stateFailed},
		// A running daemon settles it: whatever failed before is moot.
		{"running wins over a past failure", agentStatus{running: true}, false, true, stateRunning},
		// A real crash is reported as a crash, never masked by a failed control.
		{"crash wins over failed", agentStatus{crashed: true, pid: 9}, false, true, stateCrashed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := displayState(tt.status, tt.paused, tt.failed, false); got != tt.want {
				t.Errorf("displayState(%+v, paused=%v, failed=%v) = %v, want %v", tt.status, tt.paused, tt.failed, got, tt.want)
			}
		})
	}
}

func TestBuildStateIcons(t *testing.T) {
	icons := buildStateIcons(trayIcon)

	base, err := png.Decode(bytes.NewReader(trayIcon))
	if err != nil {
		t.Fatalf("embedded icon must be a valid PNG: %v", err)
	}

	for _, st := range []trayState{stateRunning, stateDownloading, statePaused, stateStopped, stateCrashed, stateFailed} {
		data := icons[st]
		if len(data) == 0 {
			t.Fatalf("state %v: empty icon", st)
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("state %v: not a decodable PNG: %v", st, err)
		}
		if img.Bounds() != base.Bounds() {
			t.Errorf("state %v: bounds %v != base %v", st, img.Bounds(), base.Bounds())
		}
	}

	// Variants carrying a badge/grayscale must differ from the base logo —
	// including downloading, whose green badge sits on the colored logo.
	for _, st := range []trayState{stateDownloading, statePaused, stateStopped, stateCrashed} {
		if bytes.Equal(icons[st], trayIcon) {
			t.Errorf("state %v: icon identical to base — badge/grayscale not applied", st)
		}
	}
	// And paused (amber) must differ from crashed (red).
	if bytes.Equal(icons[statePaused], icons[stateCrashed]) {
		t.Error("paused and crashed icons are identical")
	}
}

func TestBuildStateIconsBadPNGFallsBack(t *testing.T) {
	icons := buildStateIcons([]byte("not a png"))
	for _, st := range []trayState{stateRunning, stateDownloading, statePaused, stateStopped, stateCrashed, stateFailed} {
		if string(icons[st]) != "not a png" {
			t.Errorf("state %v: fallback must return the base bytes untouched", st)
		}
	}
}

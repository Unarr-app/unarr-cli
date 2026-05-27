package sentry

import (
	"fmt"
	"testing"

	"github.com/torrentclaw/unarr/internal/agent"
)

func TestEnvironment(t *testing.T) {
	tests := []struct {
		version string
		want    string
	}{
		{"", "development"},
		{"dev", "development"},
		{"0.1.0-dev", "development"},
		{"1.0.0", "production"},
		{"0.3.5", "production"},
		{"2.0.0-beta", "production"},
	}

	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			got := environment(tt.version)
			if got != tt.want {
				t.Errorf("environment(%q) = %q, want %q", tt.version, got, tt.want)
			}
		})
	}
}

func TestInitNoOp(t *testing.T) {
	// With empty dsn (default in tests), Init should be a no-op
	Init("1.0.0")
	// Should not panic
}

func TestCloseNoOp(t *testing.T) {
	// Close should be safe to call without Init
	Close()
}

func TestCaptureErrorNil(t *testing.T) {
	// Should not panic with nil error
	CaptureError(nil, "test")
}

func TestSetUser(t *testing.T) {
	// Should not panic without initialization
	SetUser("agent-123")
}

func TestIsUserInputErrorDaemonNotRunning(t *testing.T) {
	if !isUserInputError(agent.ErrDaemonNotRunning) {
		t.Error("ErrDaemonNotRunning should be treated as user-input error")
	}
	wrapped := fmt.Errorf("stop daemon: %w", agent.ErrDaemonNotRunning)
	if !isUserInputError(wrapped) {
		t.Error("wrapped ErrDaemonNotRunning should be treated as user-input error")
	}
}

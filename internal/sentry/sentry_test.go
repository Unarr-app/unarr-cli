package sentry

import (
	"errors"
	"fmt"
	"testing"
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

func TestShouldSkipSentryDaemonNotRunning(t *testing.T) {
	// String must stay in sync with agent.ErrDaemonNotRunning. If that sentinel
	// is reworded, this test fails loudly so the marker can be updated.
	err := errors.New("daemon does not appear to be running (state file not found)")
	if !shouldSkipSentry(err) {
		t.Error("ErrDaemonNotRunning message should be skipped")
	}
	wrapped := fmt.Errorf("read daemon state: %w", err)
	if !shouldSkipSentry(wrapped) {
		t.Error("wrapped ErrDaemonNotRunning message should be skipped")
	}
}

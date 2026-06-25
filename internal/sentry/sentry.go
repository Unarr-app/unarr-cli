package sentry

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"time"

	gosentry "github.com/getsentry/sentry-go"
	"github.com/spf13/pflag"
)

// dsn is injected at build time via ldflags. If empty, Sentry is disabled.
// Set via: -ldflags "-X github.com/Unarr-app/unarr-cli/internal/sentry.dsn=..."
var dsn string

const flushTimeout = 2 * time.Second

// Init initializes the Sentry SDK. Call Close() on shutdown to flush events.
// No-op if telemetry is disabled (UNARR_NO_TELEMETRY=1).
func Init(version string) {
	if dsn == "" || os.Getenv("UNARR_NO_TELEMETRY") == "1" {
		return
	}

	err := gosentry.Init(gosentry.ClientOptions{
		Dsn:              dsn,
		Release:          "unarr@" + version,
		Environment:      environment(version),
		AttachStacktrace: true,
	})
	if err != nil {
		return
	}

	gosentry.ConfigureScope(func(scope *gosentry.Scope) {
		scope.SetTag("os", runtime.GOOS)
		scope.SetTag("arch", runtime.GOARCH)
		scope.SetTag("go_version", runtime.Version())
	})
}

// Close flushes pending events with a timeout.
func Close() {
	gosentry.Flush(flushTimeout)
}

// daemonNotRunningMarker matches the message of agent.ErrDaemonNotRunning
// without importing the agent package — avoids a sentry → agent dependency
// that would risk a cycle if agent ever needed to report errors itself.
const daemonNotRunningMarker = "daemon does not appear to be running"

// CaptureError sends a non-fatal error to Sentry with optional command context.
// Expected non-bug errors (bad CLI input, daemon not running) are skipped to
// keep the issue feed signal-heavy.
func CaptureError(err error, command string) {
	if err == nil || shouldSkipSentry(err) {
		return
	}

	gosentry.WithScope(func(scope *gosentry.Scope) {
		if command != "" {
			scope.SetTag("command", command)
		}
		gosentry.CaptureException(err)
	})
}

func shouldSkipSentry(err error) bool {
	var notExist *pflag.NotExistError
	var valueReq *pflag.ValueRequiredError
	var invalidVal *pflag.InvalidValueError
	var invalidSyn *pflag.InvalidSyntaxError
	if errors.As(err, &notExist) || errors.As(err, &valueReq) ||
		errors.As(err, &invalidVal) || errors.As(err, &invalidSyn) {
		return true
	}
	msg := err.Error()
	return strings.HasPrefix(msg, "unknown command ") ||
		strings.HasPrefix(msg, "required flag(s)") ||
		strings.Contains(msg, daemonNotRunningMarker)
}

// RecoverPanic captures a panic and re-panics after reporting.
// Usage: defer sentry.RecoverPanic()
func RecoverPanic() {
	if r := recover(); r != nil {
		gosentry.CurrentHub().Recover(r)
		gosentry.Flush(flushTimeout)

		// Re-panic so the user sees the stack trace and the process exits non-zero
		panic(r)
	}
}

// SetUser sets the user context (agent ID) for all subsequent events.
func SetUser(agentID string) {
	gosentry.ConfigureScope(func(scope *gosentry.Scope) {
		scope.SetUser(gosentry.User{ID: agentID})
	})
}

func environment(version string) string {
	if version == "" || version == "dev" || strings.HasSuffix(version, "-dev") {
		return "development"
	}
	return "production"
}

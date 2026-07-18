package cmd

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

func TestIsValidAuthKeyFormat(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"unarr-authkey-abc123", true},
		{"unarr-authkey-X", true},
		{"unarr-authkey-", false}, // prefix only, no body
		{"unarr-authkey", false},
		{"tc_somekey", false},
		{"", false},
		{"UNARR-AUTHKEY-abc", false}, // case-sensitive prefix
	}
	for _, tt := range tests {
		if got := isValidAuthKeyFormat(tt.in); got != tt.want {
			t.Errorf("isValidAuthKeyFormat(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestRedactAuthKey(t *testing.T) {
	// Must never echo the full secret.
	full := "unarr-authkey-supersecretvalue"
	got := redactAuthKey(full)
	if strings.Contains(got, "supersecretvalue") {
		t.Errorf("redactAuthKey leaked the secret body: %q", got)
	}
	if !strings.HasPrefix(got, "unarr-au") {
		t.Errorf("redactAuthKey(%q) = %q, want prefix preserved", full, got)
	}
	if redactAuthKey("short") != "***" {
		t.Errorf("short key should redact to ***, got %q", redactAuthKey("short"))
	}
}

func TestAuthKeyErrorToken(t *testing.T) {
	tests := []struct {
		msg  string
		want string
	}{
		{"expired", "expired"},
		{"used", "used"},
		{"revoked", "revoked"},
		{"invalid", "invalid"},
		{"EXPIRED", "expired"},                               // case-insensitive
		{"auth-key has expired", "expired"},                  // substring
		{"this key was already used", "used"},                // substring
		{"the auth-key was revoked by the owner", "revoked"}, // substring
		{"something else entirely", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := authKeyErrorToken(tt.msg); got != tt.want {
			t.Errorf("authKeyErrorToken(%q) = %q, want %q", tt.msg, got, tt.want)
		}
	}
}

// TestAuthKeyExchangeError verifies each of the 4 documented server error
// tokens maps to a distinct, actionable user-facing message, and that a
// transport (non-HTTP) error is surfaced verbatim.
func TestAuthKeyExchangeError(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantSub  string // substring expected in the mapped message
		wantHint bool   // expect the "unarr.app" actionable hint
	}{
		// Status codes match the web contract (exchange/route.ts): invalid → 401,
		// expired/used/revoked → 409. Mapping keys on the {error} token, not status.
		{"expired", &agent.HTTPError{StatusCode: http.StatusConflict, Message: "expired"}, "expired", true},
		{"used", &agent.HTTPError{StatusCode: http.StatusConflict, Message: "used"}, "already used", true},
		{"revoked", &agent.HTTPError{StatusCode: http.StatusConflict, Message: "revoked"}, "revoked", true},
		{"invalid", &agent.HTTPError{StatusCode: http.StatusUnauthorized, Message: "invalid"}, "invalid", true},
		{"unknown-http", &agent.HTTPError{StatusCode: http.StatusInternalServerError, Message: "boom"}, "HTTP 500", false},
		{"transport", fmt.Errorf("dial tcp: connection refused"), "connection refused", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := authKeyExchangeError(tc.err)
			if got == nil {
				t.Fatal("expected non-nil error")
			}
			msg := got.Error()
			if !strings.Contains(msg, tc.wantSub) {
				t.Errorf("message %q does not contain %q", msg, tc.wantSub)
			}
			if tc.wantHint && !strings.Contains(msg, "unarr.app") {
				t.Errorf("message %q missing actionable unarr.app hint", msg)
			}
		})
	}
}

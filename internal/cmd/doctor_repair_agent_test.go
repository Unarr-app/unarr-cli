package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
)

func TestClassifyAuthError(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantLogin bool // message carries the `unarr login` remedy
	}{
		{"nil", nil, false},
		{"plain error", errors.New("connection refused"), false},
		{"401 rejected", &agent.HTTPError{StatusCode: 401, Message: "invalid api key"}, true},
		{"403 rejected", &agent.HTTPError{StatusCode: 403, Message: "agent_key_mismatch"}, true},
		{"500 not auth", &agent.HTTPError{StatusCode: 500, Message: "boom"}, false},
		{"404 not auth", &agent.HTTPError{StatusCode: 404, Message: "not found"}, false},
		{"wrapped 401", fmt.Errorf("register: %w", &agent.HTTPError{StatusCode: 401, Message: "nope"}), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyAuthError(tt.err)
			hasLogin := got != nil && strings.Contains(got.Error(), "unarr login")
			if hasLogin != tt.wantLogin {
				t.Errorf("classifyAuthError(%v) = %v, wantLogin=%v", tt.err, got, tt.wantLogin)
			}
			if tt.err != nil && got == nil {
				t.Errorf("non-nil error must stay non-nil")
			}
			// The original error must remain unwrappable for IsRevoked etc.
			if tt.wantLogin {
				var he *agent.HTTPError
				if !errors.As(got, &he) {
					t.Errorf("classified error lost the underlying *agent.HTTPError")
				}
			}
		})
	}
}

func TestPlanAgentRegistrationRepair_Conditions(t *testing.T) {
	tests := []struct {
		name    string
		apiKey  string
		agentID string
		want    bool
	}{
		{"no key, no id → nothing to do", "", "", false},
		{"key + id → already registered", "tc_dummy", "some-uuid", false},
		{"no key + id → nothing to do", "", "some-uuid", false},
		{"key + no id → repair", "tc_dummy", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Auth.APIKey = tt.apiKey
			cfg.Agent.ID = tt.agentID
			got := planAgentRegistrationRepair(&cfg) != nil
			if got != tt.want {
				t.Errorf("planAgentRegistrationRepair = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestApplyAgentRegistration_SuccessPersistsIdentity(t *testing.T) {
	var gotAuth, gotAgentID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/agent/register" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		var req agent.RegisterRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotAgentID = req.AgentID
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success":  true,
			"agentKey": "tc_minted_per_machine",
			"user":     map[string]any{"name": "Test", "email": "t@example.com", "plan": "pro"},
		})
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.Auth.APIURL = srv.URL
	cfg.Auth.APIKey = "tc_dummy"
	cfg.Agent.ID = ""

	if err := applyAgentRegistration(&cfg); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if cfg.Agent.ID == "" || cfg.Agent.ID != gotAgentID {
		t.Errorf("agent id not persisted/mismatched: cfg=%q server-saw=%q", cfg.Agent.ID, gotAgentID)
	}
	if cfg.Agent.Name == "" {
		t.Errorf("agent name not defaulted to hostname")
	}
	// Manual-paste bootstrap: the minted per-machine key replaces the general one.
	if cfg.Auth.APIKey != "tc_minted_per_machine" {
		t.Errorf("minted agentKey not persisted, key = %q", cfg.Auth.APIKey)
	}
	if !strings.Contains(gotAuth, "tc_dummy") {
		t.Errorf("register did not authenticate with the configured key (auth header %q)", gotAuth)
	}

	// Idempotent: a registered config plans no further registration repair.
	if planAgentRegistrationRepair(&cfg) != nil {
		t.Errorf("registration repair still planned after success")
	}
}

func TestApplyAgentRegistration_RejectedKeyLeavesConfigUntouched(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid api key"})
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.Auth.APIURL = srv.URL
	cfg.Auth.APIKey = "tc_dummy"

	err := applyAgentRegistration(&cfg)
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if !strings.Contains(err.Error(), "unarr login") {
		t.Errorf("401 not classified with the login remedy: %v", err)
	}
	if cfg.Agent.ID != "" {
		t.Errorf("failed registration must not persist an agent id, got %q", cfg.Agent.ID)
	}
	if cfg.Auth.APIKey != "tc_dummy" {
		t.Errorf("failed registration must not touch the api key, got %q", cfg.Auth.APIKey)
	}
}

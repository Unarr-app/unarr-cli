package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExchangeAuthKey_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/internal/agent/authkey/exchange" {
			t.Errorf("path = %s, want /api/internal/agent/authkey/exchange", r.URL.Path)
		}
		var req ExchangeAuthKeyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.AuthKey != "unarr-authkey-abc123" {
			t.Errorf("authKey = %q, want unarr-authkey-abc123", req.AuthKey)
		}
		if req.AgentID != "agent-xyz" {
			t.Errorf("agentId = %q, want agent-xyz", req.AgentID)
		}
		if req.Platform != "linux/amd64" {
			t.Errorf("platform = %q, want linux/amd64", req.Platform)
		}
		_ = json.NewEncoder(w).Encode(ExchangeAuthKeyResponse{
			APIKey: "tc_minted_durable_key",
			UserID: "11111111-2222-3333-4444-555555555555",
			APIURL: "https://torrentclaw.com",
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", "unarr-test")
	resp, err := c.ExchangeAuthKey(context.Background(), ExchangeAuthKeyRequest{
		AuthKey:  "unarr-authkey-abc123",
		AgentID:  "agent-xyz",
		Hostname: "nas",
		Platform: "linux/amd64",
	})
	if err != nil {
		t.Fatalf("ExchangeAuthKey failed: %v", err)
	}
	if resp.APIKey != "tc_minted_durable_key" {
		t.Errorf("apiKey = %q, want tc_minted_durable_key", resp.APIKey)
	}
	if resp.UserID == "" {
		t.Error("expected non-empty userId")
	}
	if resp.APIURL != "https://torrentclaw.com" {
		t.Errorf("apiUrl = %q, want https://torrentclaw.com", resp.APIURL)
	}
}

// TestExchangeAuthKey_Errors covers all four documented 4xx error tokens. Each
// must surface as an *HTTPError carrying the token in Message (so the cmd layer
// can map it to an actionable message) with the right status code.
func TestExchangeAuthKey_Errors(t *testing.T) {
	cases := []struct {
		token  string
		status int
	}{
		// Status codes match the web contract (exchange/route.ts): invalid → 401,
		// everything else → 409. The CLI maps on the {error} token, not status,
		// so these stay accurate documentation of what the server actually sends.
		{"invalid", http.StatusUnauthorized},
		{"expired", http.StatusConflict},
		{"used", http.StatusConflict},
		{"revoked", http.StatusConflict},
	}

	for _, tc := range cases {
		t.Run(tc.token, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(ErrorResponse{Error: tc.token})
			}))
			defer srv.Close()

			c := NewClient(srv.URL, "", "unarr-test")
			resp, err := c.ExchangeAuthKey(context.Background(), ExchangeAuthKeyRequest{
				AuthKey: "unarr-authkey-zzz",
				AgentID: "agent-xyz",
			})
			if err == nil {
				t.Fatalf("expected error for token %q, got resp %+v", tc.token, resp)
			}
			var he *HTTPError
			if !errors.As(err, &he) {
				t.Fatalf("error %v is not *HTTPError", err)
			}
			if he.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", he.StatusCode, tc.status)
			}
			if he.Message != tc.token {
				t.Errorf("message = %q, want %q", he.Message, tc.token)
			}
		})
	}
}

func TestExchangeAuthKey_EmptyKeyRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 200 but empty apiKey — must still be treated as a failure.
		_ = json.NewEncoder(w).Encode(ExchangeAuthKeyResponse{APIKey: ""})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", "unarr-test")
	if _, err := c.ExchangeAuthKey(context.Background(), ExchangeAuthKeyRequest{
		AuthKey: "unarr-authkey-zzz",
		AgentID: "agent-xyz",
	}); err == nil {
		t.Fatal("expected error on empty apiKey, got nil")
	}
}

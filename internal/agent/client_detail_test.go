package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPErrorCarriesTheServersSentence(t *testing.T) {
	// The server writes a specific, user-facing message for every rejection —
	// which plan, how many machines, what to run. The client parsed the body and
	// kept only the machine code, so none of that ever reached a user.
	const detail = "Agent limit reached for your plan (2 active agents). Upgrade to connect more."
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"agent_limit_reached","message":"` + detail + `"}`))
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "k", "test").Register(context.Background(), RegisterRequest{})

	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("err = %v, want an *HTTPError", err)
	}
	if he.Detail != detail {
		t.Errorf("Detail = %q, want the server's sentence", he.Detail)
	}
	// Message stays the machine code: callers branch on it (IsRevoked, the
	// auth-key token matcher), and changing it under them would break those.
	if he.Message != "agent_limit_reached" {
		t.Errorf("Message = %q, want the machine code preserved", he.Message)
	}
}

func TestHTTPErrorWithoutAMessageFieldIsUnchanged(t *testing.T) {
	// Older servers, and endpoints that only send a code.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
		w.Write([]byte(`{"error":"agent_revoked"}`))
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "k", "test").Register(context.Background(), RegisterRequest{})

	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("err = %v, want an *HTTPError", err)
	}
	if he.Detail != "" {
		t.Errorf("Detail = %q, want empty", he.Detail)
	}
	if !IsRevoked(err) {
		t.Error("IsRevoked() = false — the revocation path must survive the new field")
	}
}

func TestSetAPIKeyChangesWhatIsSent(t *testing.T) {
	// A parked daemon swaps in the key the user just signed in with, without a
	// restart.
	seen := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Authorization")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "old", "test")
	c.Register(context.Background(), RegisterRequest{})
	if got := <-seen; got != "Bearer old" {
		t.Fatalf("first request sent %q", got)
	}

	c.SetAPIKey("new")
	c.Register(context.Background(), RegisterRequest{})
	if got := <-seen; got != "Bearer new" {
		t.Errorf("after SetAPIKey the request sent %q, want the new key", got)
	}
}

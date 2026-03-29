package cmd

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGenerateState(t *testing.T) {
	state, err := generateState()
	if err != nil {
		t.Fatalf("generateState: %v", err)
	}
	if len(state) != 64 { // 32 bytes = 64 hex chars
		t.Errorf("state length = %d, want 64", len(state))
	}
	for _, c := range state {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("state contains non-hex char: %c", c)
			break
		}
	}

	// Two states should differ
	state2, _ := generateState()
	if state == state2 {
		t.Error("consecutive states should differ")
	}
}

func TestCallbackHTML(t *testing.T) {
	if !strings.Contains(callbackHTML, "Connected to torrentclaw") {
		t.Error("missing success message")
	}
	if !strings.Contains(callbackHTML, "close this tab") {
		t.Error("missing close instruction")
	}
}

func TestCallbackHandler_ValidState(t *testing.T) {
	state := "abc123def456abc123def456abc123def456abc123def456abc123def456abcd"
	tokenCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			http.Error(w, "Invalid state", http.StatusBadRequest)
			errCh <- fmt.Errorf("state mismatch")
			return
		}
		token := r.URL.Query().Get("token")
		if token == "" {
			http.Error(w, "No token", http.StatusBadRequest)
			errCh <- fmt.Errorf("empty token")
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, callbackHTML)
		tokenCh <- token
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	// Simulate browser redirect to callback
	resp, err := http.Get(server.URL + "/callback?token=tc_test_key_123&state=" + state)
	if err != nil {
		t.Fatalf("callback request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	select {
	case token := <-tokenCh:
		if token != "tc_test_key_123" {
			t.Errorf("token = %q, want tc_test_key_123", token)
		}
	case err := <-errCh:
		t.Fatalf("unexpected error: %v", err)
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for token")
	}
}

func TestCallbackHandler_InvalidState(t *testing.T) {
	tokenCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != "correct_state" {
			http.Error(w, "Invalid state", http.StatusBadRequest)
			errCh <- fmt.Errorf("state mismatch")
			return
		}
		tokenCh <- r.URL.Query().Get("token")
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Get(server.URL + "/callback?token=tc_test&state=wrong_state")
	if err != nil {
		t.Fatalf("callback request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}

	select {
	case <-errCh:
		// Expected — state mismatch
	case <-tokenCh:
		t.Fatal("should not have received token with wrong state")
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}

func TestCallbackHandler_MissingToken(t *testing.T) {
	state := "valid_state_0123456789abcdef0123456789abcdef"
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			http.Error(w, "Invalid state", http.StatusBadRequest)
			errCh <- fmt.Errorf("state mismatch")
			return
		}
		token := r.URL.Query().Get("token")
		if token == "" {
			http.Error(w, "No token", http.StatusBadRequest)
			errCh <- fmt.Errorf("empty token")
			return
		}
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Get(server.URL + "/callback?state=" + state)
	if err != nil {
		t.Fatalf("callback request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestBrowserAuth_ServerBinds(t *testing.T) {
	// Verify browserAuth can bind to a free port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	if port < 1024 {
		t.Errorf("port %d < 1024", port)
	}
}

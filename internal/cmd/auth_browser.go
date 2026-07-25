package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const browserAuthTimeout = 60 * time.Second

// browserAuth opens a browser for the user to authorize the CLI.
// Returns the API key on success, or an error if the flow fails/times out.
//
// Flow:
//  1. Start a temporary HTTP server on a random localhost port
//  2. Open browser to {apiURL}/cli/auth?state={state}&port={port}
//  3. User logs in and clicks "Authorize" on the web page
//  4. Web redirects to localhost:{port}/callback?token=tc_...&state={state}
//  5. CLI validates state, extracts token, closes server
func browserAuth(apiURL, agentID string) (string, error) {
	// Validate apiURL is a well-formed HTTP(S) URL
	parsed, err := url.Parse(apiURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("invalid API URL: %s", apiURL)
	}

	state, err := generateState()
	if err != nil {
		return "", fmt.Errorf("generate state: %w", err)
	}

	// Find a free port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("listen: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	// Channel to receive the token from the callback
	tokenCh := make(chan string, 1)
	errCh := make(chan error, 1)

	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		handled := false
		once.Do(func() {
			handled = true

			// Validate state to prevent CSRF
			if r.URL.Query().Get("state") != state {
				http.Error(w, "Invalid state parameter", http.StatusBadRequest)
				errCh <- fmt.Errorf("state mismatch")
				return
			}

			// Check if user rejected the authorization
			if r.URL.Query().Get("rejected") == "1" {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				fmt.Fprint(w, rejectedHTML)
				errCh <- fmt.Errorf("authorization rejected by user")
				return
			}

			token := r.URL.Query().Get("token")
			if token == "" {
				http.Error(w, "No token received", http.StatusBadRequest)
				errCh <- fmt.Errorf("empty token in callback")
				return
			}

			// Respond with a success page
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, callbackHTML)

			tokenCh <- token
		})
		if !handled {
			http.Error(w, "Already processed", http.StatusConflict)
		}
	})

	server := &http.Server{Handler: mux}

	// Start server in background
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	// Open browser. Forward the agentId so the server mints a per-machine key
	// bound to it (omitted → server falls back to the legacy general key).
	authURL := fmt.Sprintf("%s/unarr/auth?state=%s&port=%d", apiURL, url.QueryEscape(state), port)
	if agentID != "" {
		authURL += "&agentId=" + url.QueryEscape(agentID)
	}
	// Check the server answers BEFORE handing the user a browser tab. Opening a
	// URL nothing is serving shows them a browser error page and then makes
	// them wait out the full authorization timeout for a failure that was
	// knowable in a second — and the reason (a stale api_url, a server that is
	// down) never reaches them.
	if err := reachable(apiURL); err != nil {
		shutdownServer(server)
		return "", err
	}
	// ALWAYS print the URL. openBrowser is best-effort and on a headless box
	// (SSH, Docker, WSL, a NAS shell) nothing opens — with only the countdown on
	// screen the user has nothing to act on and waits out the timeout for no
	// reason. Printing it also lets them authorize from another machine.
	if err := openBrowser(authURL); err != nil {
		fmt.Println("  Could not open a browser here. Open this URL to authorize:")
	} else {
		fmt.Println("  If your browser didn't open, visit:")
	}
	fmt.Printf("  %s\n\n", authURL)

	// NOTE: no "Enter to skip" reader here. A goroutine blocked on os.Stdin.Read
	// cannot be cancelled when this function returns, so it went on to swallow
	// the first keystroke of the huh form that runs right after in `unarr init`.

	// Wait for callback with countdown
	ctx, cancel := context.WithTimeout(context.Background(), browserAuthTimeout)
	defer cancel()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	remaining := int(browserAuthTimeout.Seconds())

	// Show initial countdown
	fmt.Printf("\r  Waiting for browser authorization... %ds (Ctrl+C to cancel)  ", remaining)

	var token string
	done := false
	for !done {
		select {
		case token = <-tokenCh:
			fmt.Print("\r\033[K") // clear countdown line
			done = true
		case err := <-errCh:
			fmt.Print("\r\033[K")
			shutdownServer(server)
			return "", err
		case <-ctx.Done():
			fmt.Print("\r\033[K")
			shutdownServer(server)
			return "", fmt.Errorf("timed out waiting for browser authorization")
		case <-ticker.C:
			remaining--
			if remaining >= 0 {
				fmt.Printf("\r  Waiting for browser authorization... %ds (Ctrl+C to cancel)  ", remaining)
			}
		}
	}

	shutdownServer(server)

	return token, nil
}

// reachableTimeout bounds the pre-flight probe: long enough for a slow link,
// short enough that it is not a second wait of its own.
const reachableTimeout = 6 * time.Second

// reachable reports whether the API server answers at all. Any HTTP reply
// counts — the probe is about the server existing, not about what it says.
func reachable(apiURL string) error {
	client := &http.Client{Timeout: reachableTimeout}
	resp, err := client.Get(strings.TrimRight(apiURL, "/") + "/api/health")
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w\nCheck the agent's api_url (unarr config) or your connection", apiURL, err)
	}
	defer resp.Body.Close()
	return nil
}

func shutdownServer(server *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}

func generateState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// rejectedHTML is the page shown in the browser when the user clicks Cancel.
const rejectedHTML = `<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <title>unarr — Cancelled</title>
  <style>
    body { font-family: -apple-system, system-ui, sans-serif; display: flex; justify-content: center; align-items: center; min-height: 100vh; margin: 0; background: #0a0a0a; color: #fafafa; }
    .card { text-align: center; padding: 3rem; }
    .icon { font-size: 4rem; margin-bottom: 1rem; }
    h1 { font-size: 1.5rem; margin-bottom: 0.5rem; }
    p { color: #888; font-size: 0.95rem; }
  </style>
</head>
<body>
  <div class="card">
    <div class="icon">—</div>
    <h1>Authorization cancelled</h1>
    <p>You can close this tab and run the command again in your terminal.</p>
  </div>
</body>
</html>`

// callbackHTML is the page shown in the browser after successful authorization.
const callbackHTML = `<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <title>unarr — Connected</title>
  <style>
    body { font-family: -apple-system, system-ui, sans-serif; display: flex; justify-content: center; align-items: center; min-height: 100vh; margin: 0; background: #0a0a0a; color: #fafafa; }
    .card { text-align: center; padding: 3rem; }
    .check { font-size: 4rem; margin-bottom: 1rem; }
    h1 { font-size: 1.5rem; margin-bottom: 0.5rem; }
    p { color: #888; font-size: 0.95rem; }
  </style>
</head>
<body>
  <div class="card">
    <div class="check">✓</div>
    <h1>Connected to unarr</h1>
    <p>You can close this tab and return to your terminal.</p>
  </div>
</body>
</html>`

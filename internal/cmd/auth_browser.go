package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const browserAuthTimeout = 5 * time.Minute

// browserAuth opens a browser for the user to authorize the CLI.
// Returns the API key on success, or an error if the flow fails/times out.
//
// Flow:
//  1. Start a temporary HTTP server on a random localhost port
//  2. Open browser to {apiURL}/cli/auth?state={state}&port={port}
//  3. User logs in and clicks "Authorize" on the web page
//  4. Web redirects to localhost:{port}/callback?token=tc_...&state={state}
//  5. CLI validates state, extracts token, closes server
func browserAuth(apiURL string) (string, error) {
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

	// Open browser
	authURL := fmt.Sprintf("%s/cli/auth?state=%s&port=%d", apiURL, url.QueryEscape(state), port)
	openBrowser(authURL)

	// Wait for callback, error, or timeout
	ctx, cancel := context.WithTimeout(context.Background(), browserAuthTimeout)
	defer cancel()

	var token string
	select {
	case token = <-tokenCh:
		// Success
	case err := <-errCh:
		shutdownCtx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
		_ = server.Shutdown(shutdownCtx2)
		cancel2()
		return "", err
	case <-ctx.Done():
		shutdownCtx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
		_ = server.Shutdown(shutdownCtx2)
		cancel2()
		return "", fmt.Errorf("timed out waiting for browser authorization")
	}

	// Shutdown the server
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()
	_ = server.Shutdown(shutdownCtx)

	return token, nil
}

func generateState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

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
    <h1>Connected to torrentclaw</h1>
    <p>You can close this tab and return to your terminal.</p>
  </div>
</body>
</html>`

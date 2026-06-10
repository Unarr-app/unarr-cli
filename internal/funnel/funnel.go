// Package funnel manages the optional CloudFlare Quick Tunnel subprocess
// that gives the daemon a public HTTPS hostname for cross-network playback
// from browser-based clients (web player on torrentclaw.com / torrentclaw.to).
//
// Why: HTTPS pages can't fetch HTTP resources (mixed content). Without a
// tunnel the daemon is only reachable from the same machine (localhost is
// exempt) or via Tailscale (which users can install themselves but most
// won't). CF Quick Tunnels are anonymous — no CF account, no DNS, no port
// forwarding — and assign a one-shot `https://<random>.trycloudflare.com`
// URL. Bytes flow through CF, never through our infra (legal posture: we
// don't relay; CF does).
//
// Lifecycle:
//
//	t, err := funnel.Start(ctx, funnel.Config{Port: 11819})
//	defer t.Close()
//	url, err := t.WaitURL(30 * time.Second)  // blocks until cloudflared emits the URL
//
// The tunnel runs until the context is cancelled or t.Close() is called.
package funnel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sync"
	"time"
)

// urlPattern matches the `https://<random>.trycloudflare.com` URL cloudflared
// prints when a Quick Tunnel is registered. Quick Tunnel hostnames are always
// several hyphen-joined dictionary words (e.g.
// `make-appointments-negotiation-blacks`), so we require at least one hyphen.
// This deliberately excludes cloudflared's control-plane endpoint
// `https://api.trycloudflare.com`, which appears earlier in the log stream — a
// permissive `[a-z0-9-]+` matched `api` first and we advertised a dead URL.
var urlPattern = regexp.MustCompile(`https://[a-z0-9]+(?:-[a-z0-9]+)+\.trycloudflare\.com`)

// Config controls how the tunnel is launched.
type Config struct {
	// Port is the local upstream port cloudflared will tunnel to. Required.
	Port int
	// Binary is the cloudflared executable path. When empty the package looks
	// it up via $PATH.
	Binary string
}

// Tunnel is a handle on a running cloudflared Quick Tunnel.
type Tunnel struct {
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	urlCh   chan string
	exitCh  chan error
	mu      sync.Mutex
	url     string
	stopped bool
}

// Start launches cloudflared as a subprocess. The returned *Tunnel exposes the
// public URL via WaitURL once cloudflared registers it (usually 2–5 s).
//
// The subprocess inherits the cancellation of the supplied context. Closing
// the *Tunnel sends SIGTERM and waits for the subprocess to exit.
func Start(ctx context.Context, cfg Config) (*Tunnel, error) {
	if cfg.Port <= 0 {
		return nil, fmt.Errorf("funnel: invalid Port %d", cfg.Port)
	}
	binary := cfg.Binary
	if binary == "" {
		resolved, err := ResolveBinary()
		if err != nil {
			return nil, err
		}
		binary = resolved
	}

	subCtx, cancel := context.WithCancel(ctx)
	// `--no-autoupdate` disables cloudflared's daily self-update check (the
	// daemon manages binary rotation). `--metrics 127.0.0.1:0` suppresses the
	// default `:9090` listener that would collide on a shared box.
	cmd := exec.CommandContext(subCtx, binary,
		"tunnel",
		"--no-autoupdate",
		"--metrics", "127.0.0.1:0",
		"--url", fmt.Sprintf("http://localhost:%d", cfg.Port),
	)

	// cloudflared writes the connect log + assigned URL to stderr.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("funnel: pipe stderr: %w", err)
	}
	cmd.Stdout = io.Discard // quick tunnels print nothing useful on stdout

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("funnel: start cloudflared: %w", err)
	}

	t := &Tunnel{
		cmd:    cmd,
		cancel: cancel,
		urlCh:  make(chan string, 1),
		exitCh: make(chan error, 1),
	}

	// Reader goroutine: scan cloudflared's stderr for the URL, surface the
	// rest as a single string we don't try to interpret.
	go t.scanStderr(stderr)

	// Waiter goroutine: signal exit so callers can react (e.g. restart).
	go func() {
		t.exitCh <- cmd.Wait()
	}()

	return t, nil
}

// WaitURL blocks until cloudflared has registered the tunnel and emitted the
// public URL, or `timeout` elapses, or the subprocess exits. The returned URL
// has the form `https://<random>.trycloudflare.com`.
func (t *Tunnel) WaitURL(timeout time.Duration) (string, error) {
	t.mu.Lock()
	if t.url != "" {
		u := t.url
		t.mu.Unlock()
		return u, nil
	}
	t.mu.Unlock()

	select {
	case u := <-t.urlCh:
		return u, nil
	case err := <-t.exitCh:
		if err == nil {
			return "", errors.New("funnel: cloudflared exited before URL")
		}
		return "", fmt.Errorf("funnel: cloudflared exited: %w", err)
	case <-time.After(timeout):
		return "", fmt.Errorf("funnel: timed out waiting for URL after %s", timeout)
	}
}

// URL returns the assigned tunnel URL, or "" if not yet emitted.
func (t *Tunnel) URL() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.url
}

// Done returns a channel that closes once the subprocess exits. The error sent
// before close describes the exit reason (nil = clean shutdown via Close).
func (t *Tunnel) Done() <-chan error {
	return t.exitCh
}

// Close terminates the subprocess and waits for it to exit. Safe to call
// multiple times.
func (t *Tunnel) Close() error {
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return nil
	}
	t.stopped = true
	t.mu.Unlock()
	t.cancel()
	// Drain the exit channel so the Wait goroutine doesn't leak.
	select {
	case <-t.exitCh:
	case <-time.After(5 * time.Second):
	}
	return nil
}

func (t *Tunnel) scanStderr(r io.Reader) {
	scanner := bufio.NewScanner(r)
	// Some cloudflared lines exceed the default 64KiB scanner buffer (when it
	// prints connection diagnostics). Bump to 1MiB.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if t.URL() == "" {
			if m := urlPattern.FindString(line); m != "" {
				t.mu.Lock()
				t.url = m
				t.mu.Unlock()
				// Non-blocking send: if no one is listening, just drop —
				// the URL field carries the value for any later WaitURL call.
				select {
				case t.urlCh <- m:
				default:
				}
			}
		}
	}
}

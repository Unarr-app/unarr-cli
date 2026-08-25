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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"regexp"
	"sync"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/winproc"
)

// urlPattern matches the `https://<random>.trycloudflare.com` URL cloudflared
// prints when a Quick Tunnel is registered. Quick Tunnel hostnames are always
// several hyphen-joined dictionary words (e.g.
// `make-appointments-negotiation-blacks`), so we require at least one hyphen.
// This deliberately excludes cloudflared's control-plane endpoint
// `https://api.trycloudflare.com`, which appears earlier in the log stream — a
// permissive `[a-z0-9-]+` matched `api` first and we advertised a dead URL.
var urlPattern = regexp.MustCompile(`https://[a-z0-9]+(?:-[a-z0-9]+)+\.trycloudflare\.com`)

// metricsPattern matches the line cloudflared logs when its metrics listener
// binds — `Starting metrics server on 127.0.0.1:20241/metrics`. We launch it on
// `127.0.0.1:0` (an ephemeral port, to never collide on a shared box), so this
// line is the only way to learn the port and reach GET /ready, which reports
// how many connections to the CF edge the connector currently holds.
var metricsPattern = regexp.MustCompile(`Starting metrics server on (127\.0\.0\.1:\d+)/metrics`)

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
	metrics string // host:port of cloudflared's metrics listener, "" until seen
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
	winproc.HideWindow(cmd)

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

// MetricsAddr returns the host:port of cloudflared's metrics listener, or ""
// if its bind line has not been seen (older/newer cloudflared wording, or the
// process died first). Callers must degrade: the listener is a convenience,
// not a contract.
func (t *Tunnel) MetricsAddr() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.metrics
}

// ErrNoMetrics is returned by Ready when the metrics listener address is unknown.
var ErrNoMetrics = errors.New("funnel: cloudflared metrics address not seen")

// Ready asks cloudflared's own readiness endpoint how many connections to the
// CF edge it holds. 0 means the connector is cut off from the edge (it keeps
// retrying internally and never exits on its own). This is a LOCAL signal —
// it says nothing about whether the public hostname still routes anywhere.
func (t *Tunnel) Ready(ctx context.Context) (readyConnections int, err error) {
	addr := t.MetricsAddr()
	if addr == "" {
		return 0, ErrNoMetrics
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/ready", nil)
	if err != nil {
		return 0, err
	}
	res, err := readyClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	// 200 = connections > 0, 503 = none; the body carries the count either way.
	var body struct {
		ReadyConnections int `json:"readyConnections"`
	}
	if derr := json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&body); derr != nil {
		if res.StatusCode == http.StatusServiceUnavailable {
			return 0, nil
		}
		return 0, fmt.Errorf("funnel: /ready %d: %w", res.StatusCode, derr)
	}
	return body.ReadyConnections, nil
}

var readyClient = &http.Client{Timeout: 3 * time.Second}

// Kill terminates the subprocess WITHOUT waiting or draining Done: the exit
// still arrives on Done for whoever is blocked there (the supervisor), which
// is exactly what a watchdog goroutine needs — Close from a second goroutine
// would race the supervisor for the single exit value and strand it.
func (t *Tunnel) Kill() { t.cancel() }

// Close terminates the subprocess (exec.CommandContext kills it outright —
// cloudflared has nothing to flush) and waits for it to exit. Safe to call
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
		t.scanLine(scanner.Text())
	}
}

// scanLine folds one stderr line into the tunnel's state. Every line is
// checked for BOTH patterns: the metrics bind line comes before the URL
// banner, and an early return on "URL already known" used to make the scanner
// blind to everything after the URL.
func (t *Tunnel) scanLine(line string) {
	if m := metricsPattern.FindStringSubmatch(line); m != nil {
		t.mu.Lock()
		if t.metrics == "" {
			t.metrics = m[1]
		}
		t.mu.Unlock()
	}
	m := urlPattern.FindString(line)
	if m == "" {
		return
	}
	t.mu.Lock()
	if t.url != "" {
		known := t.url
		t.mu.Unlock()
		// A quick tunnel registers its hostname once; cloudflared reconnects to
		// the SAME tunnel and never mints a second name in-process. If that ever
		// changes, say so — the supervisor's model is "new URL = new process".
		if known != m {
			log.Printf("[funnel] cloudflared printed a second hostname (%s) after %s - ignored", m, known)
		}
		return
	}
	t.url = m
	t.mu.Unlock()
	// Non-blocking send: if no one is listening, just drop —
	// the URL field carries the value for any later WaitURL call.
	select {
	case t.urlCh <- m:
	default:
	}
}

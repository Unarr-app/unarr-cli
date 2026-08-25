package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeProber is the cloudflared /ready stand-in.
type fakeProber struct {
	n   int
	err error
}

func (f fakeProber) Ready(context.Context) (int, error) { return f.n, f.err }

// scripted returns outcomes in order, repeating the last one forever.
func scripted(outcomes ...probeOutcome) func(context.Context, string) (probeOutcome, string) {
	i := 0
	return func(context.Context, string) (probeOutcome, string) {
		o := outcomes[min(i, len(outcomes)-1)]
		i++
		d := ""
		if o == probeFailed {
			d = "scripted failure"
		}
		return o, d
	}
}

func always(o probeOutcome, detail string) func(context.Context, string) (probeOutcome, string) {
	return func(context.Context, string) (probeOutcome, string) { return o, detail }
}

// fast builds a watcher whose clock is milliseconds, so a verdict that takes
// 60-90 s in production takes a few ms here.
func fast(lookup, get func(context.Context, string) (probeOutcome, string), p funnelProber) *funnelHealth {
	return &funnelHealth{every: 2 * time.Millisecond, failLimit: 3, minLifetime: 0, get: get, lookup: lookup, prober: p}
}

// watchFor runs watch with a deadline and reports the verdict ("" = none in time).
func watchFor(t *testing.T, h *funnelHealth, d time.Duration, startedAt time.Time) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return h.watch(ctx, "https://word-word-word.trycloudflare.com", startedAt)
}

// TestWatchNXDOMAINIsDeath is the field failure, stated as a test: the process
// is alive, the hostname is gone, and until now nobody noticed.
func TestWatchNXDOMAINIsDeath(t *testing.T) {
	h := fast(always(probeFailed, "hostname is NXDOMAIN"), always(probeIgnored, ""), nil)
	got := watchFor(t, h, time.Second, time.Now().Add(-time.Hour))
	if !strings.Contains(got, "NXDOMAIN") {
		t.Fatalf("want an NXDOMAIN verdict, got %q", got)
	}
}

// TestWatchEdge530IsDeath: the phase BEFORE NXDOMAIN — the hostname still
// resolves and CF answers an error page for it. A status-agnostic probe would
// have called that alive.
func TestWatchEdge530IsDeath(t *testing.T) {
	h := fast(always(probeIgnored, ""), always(probeFailed, "GET /health answered 530"), nil)
	got := watchFor(t, h, time.Second, time.Now().Add(-time.Hour))
	if !strings.Contains(got, "530") {
		t.Fatalf("want the 530 verdict, got %q", got)
	}
}

// TestWatchResolverTroubleIsNotDeath: SERVFAIL / resolver timeouts and CF 429s
// say nothing about the tunnel. The watcher must sit through them forever
// rather than restart a healthy tunnel because the user's DNS hiccuped.
func TestWatchResolverTroubleIsNotDeath(t *testing.T) {
	h := fast(always(probeIgnored, ""), always(probeIgnored, ""), fakeProber{n: 2})
	if got := watchFor(t, h, 50*time.Millisecond, time.Now().Add(-time.Hour)); got != "" {
		t.Fatalf("ignored outcomes must never produce a verdict, got %q", got)
	}
}

// TestWatchHealthyAnswerResetsTheStreak: two failures, the daemon answers,
// two more failures — never three in a row, never a verdict.
func TestWatchHealthyAnswerResetsTheStreak(t *testing.T) {
	get := scripted(probeFailed, probeFailed, probeHealthy, probeFailed, probeFailed, probeHealthy)
	h := fast(always(probeIgnored, ""), get, nil)
	if got := watchFor(t, h, 40*time.Millisecond, time.Now().Add(-time.Hour)); got != "" {
		t.Fatalf("a healthy answer must reset the streak, got %q", got)
	}
}

// TestWatchNeverKillsAYoungTunnel: a hostname minted seconds ago may not have
// propagated yet; three early NXDOMAINs are not a verdict.
func TestWatchNeverKillsAYoungTunnel(t *testing.T) {
	h := fast(always(probeFailed, "hostname is NXDOMAIN"), always(probeIgnored, ""), nil)
	h.minLifetime = time.Hour
	if got := watchFor(t, h, 50*time.Millisecond, time.Now()); got != "" {
		t.Fatalf("no verdict before minLifetime, got %q", got)
	}
}

// TestWatchReadyZeroIsATieBreakOnly: cloudflared's own "0 edge connections"
// counts only when DNS and the end-to-end GET said nothing.
func TestWatchReadyZeroIsATieBreakOnly(t *testing.T) {
	h := fast(always(probeIgnored, ""), always(probeIgnored, ""), fakeProber{n: 0})
	got := watchFor(t, h, time.Second, time.Now().Add(-time.Hour))
	if !strings.Contains(got, "0 edge connections") {
		t.Fatalf("want the /ready verdict, got %q", got)
	}
	// ...but never when the daemon is answering through the tunnel.
	h = fast(always(probeIgnored, ""), always(probeHealthy, ""), fakeProber{n: 0})
	if got := watchFor(t, h, 30*time.Millisecond, time.Now().Add(-time.Hour)); got != "" {
		t.Fatalf("a healthy GET outranks /ready, got %q", got)
	}
	// A prober that errors (no metrics port seen) is simply no signal.
	h = fast(always(probeIgnored, ""), always(probeIgnored, ""), fakeProber{err: errors.New("no metrics")})
	if got := watchFor(t, h, 30*time.Millisecond, time.Now().Add(-time.Hour)); got != "" {
		t.Fatalf("a failing prober is not a failing tunnel, got %q", got)
	}
}

// TestWatchStopsOnCancel: the watcher must return promptly when its tunnel is
// gone, or the supervisor leaks one goroutine per rotation.
func TestWatchStopsOnCancel(t *testing.T) {
	h := fast(always(probeIgnored, ""), always(probeIgnored, ""), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan string, 1)
	go func() { done <- h.watch(ctx, "https://a-b.trycloudflare.com", time.Now()) }()
	cancel()
	select {
	case got := <-done:
		if got != "" {
			t.Fatalf("cancellation is not a verdict, got %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("watch did not return after cancel")
	}
}

// TestClassifyHealthResponse pins the response-shape rule: only the daemon's
// own JSON is health; everything CF says on the daemon's behalf is not.
func TestClassifyHealthResponse(t *testing.T) {
	cases := []struct {
		name  string
		code  int
		ctype string
		body  string
		want  probeOutcome
	}{
		{"daemon health", 200, "application/json; charset=utf-8", `{"status":"ok","streaming":false,"port":11818}`, probeHealthy},
		{"CF 530 error page", 530, "text/html; charset=UTF-8", "<html>Origin unreachable</html>", probeFailed},
		{"CF 502", 502, "text/html", "<html/>", probeFailed},
		{"CF throttle", 429, "text/html", "rate limited", probeIgnored},
		{"200 but HTML (captive portal)", 200, "text/html", "<html/>", probeFailed},
		{"JSON without status (not our daemon)", 200, "application/json", `{"ok":true}`, probeFailed},
		{"JSON header on a lying body", 200, "application/json", "<html/>", probeFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, detail := classifyHealthResponse(tc.code, tc.ctype, strings.NewReader(tc.body))
			if got != tc.want {
				t.Fatalf("got %v (%q), want %v", got, detail, tc.want)
			}
			for i, b := range []byte(detail) {
				if b > 0x7F {
					t.Fatalf("non-ASCII byte %#x at %d in verdict %q", b, i, detail)
				}
			}
		})
	}
}

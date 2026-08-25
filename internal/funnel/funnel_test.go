package funnel

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestURLPattern(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{
			name: "real quick tunnel banner",
			line: "2026-05-29T22:18:33Z INF |  https://make-appointments-negotiation-blacks.trycloudflare.com  |",
			want: "https://make-appointments-negotiation-blacks.trycloudflare.com",
		},
		{
			name: "two-word hostname",
			line: "https://blue-river.trycloudflare.com is ready",
			want: "https://blue-river.trycloudflare.com",
		},
		{
			name: "control-plane api endpoint is ignored",
			line: `2026-05-29T22:17:59Z DBG POST https://api.trycloudflare.com/tunnel`,
			want: "",
		},
		{
			name: "no trycloudflare url",
			line: "2026-05-29T22:17:44Z INF Requesting new quick Tunnel on trycloudflare.com...",
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := urlPattern.FindString(tc.line); got != tc.want {
				t.Fatalf("FindString(%q) = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

func TestMetricsPattern(t *testing.T) {
	line := "2026-08-25T17:01:02Z INF Starting metrics server on 127.0.0.1:20241/metrics"
	m := metricsPattern.FindStringSubmatch(line)
	if m == nil || m[1] != "127.0.0.1:20241" {
		t.Fatalf("metricsPattern on %q = %v", line, m)
	}
	if metricsPattern.MatchString("INF Requesting new quick Tunnel on trycloudflare.com...") {
		t.Fatal("must not match unrelated lines")
	}
}

// TestScanLineSeesEverything: the old scanner stopped looking after the first
// URL, so the metrics line (which comes FIRST) was the only thing it ever saw
// before going blind — and a second hostname would have been silently dropped.
func TestScanLineSeesEverything(t *testing.T) {
	tun := &Tunnel{urlCh: make(chan string, 1)}
	tun.scanLine("2026-08-25T17:01:02Z INF Starting metrics server on 127.0.0.1:20241/metrics")
	tun.scanLine("2026-08-25T17:01:05Z INF |  https://make-appointments-negotiation-blacks.trycloudflare.com  |")
	tun.scanLine("2026-08-25T17:01:06Z INF |  https://other-other.trycloudflare.com  |") // anomaly, ignored
	if got := tun.MetricsAddr(); got != "127.0.0.1:20241" {
		t.Fatalf("MetricsAddr = %q", got)
	}
	if got := tun.URL(); got != "https://make-appointments-negotiation-blacks.trycloudflare.com" {
		t.Fatalf("URL = %q (the first hostname must win)", got)
	}
	select {
	case u := <-tun.urlCh:
		if u != tun.URL() {
			t.Fatalf("urlCh carried %q", u)
		}
	default:
		t.Fatal("the URL was never published on urlCh")
	}
	// Order does not matter: the URL before the metrics line is captured too.
	tun = &Tunnel{urlCh: make(chan string, 1)}
	tun.scanLine("https://blue-river.trycloudflare.com is ready")
	tun.scanLine("INF Starting metrics server on 127.0.0.1:1/metrics")
	if tun.URL() == "" || tun.MetricsAddr() == "" {
		t.Fatalf("URL=%q metrics=%q", tun.URL(), tun.MetricsAddr())
	}
}

func TestReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ready" {
			http.NotFound(w, r)
			return
		}
		switch r.URL.RawQuery {
		case "dead":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":503,"readyConnections":0,"connectorId":"x"}`))
		default:
			_, _ = w.Write([]byte(`{"status":200,"readyConnections":4,"connectorId":"x"}`))
		}
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	tun := &Tunnel{metrics: addr}
	if n, err := tun.Ready(context.Background()); err != nil || n != 4 {
		t.Fatalf("Ready = %d, %v; want 4, nil", n, err)
	}
	// No metrics line seen → no signal, not a false "dead".
	if _, err := (&Tunnel{}).Ready(context.Background()); !errors.Is(err, ErrNoMetrics) {
		t.Fatalf("Ready without metrics addr = %v, want ErrNoMetrics", err)
	}
}

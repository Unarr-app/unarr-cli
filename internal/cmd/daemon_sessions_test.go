package cmd

import (
	"bytes"
	"context"
	"log"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/engine"
	"github.com/Unarr-app/unarr-cli/internal/metrics"
)

// freeAddr returns a loopback address that is free right now.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func listening(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// The feature is opt-in: with [metrics] enabled=false (the default) nothing may
// listen, even when an address is configured.
func TestStartSessionViewDisabledBindsNoPort(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := startSessionView(ctx, metrics.NewHub("v", nil), config.MetricsConfig{Listen: addr}, "ctrl-token")
	if srv != nil {
		t.Fatal("disabled metrics returned a server")
	}
	if listening(addr) {
		t.Fatalf("%s is listening although [metrics] is disabled", addr)
	}
	// The default address must stay closed too.
	if got := config.Default().Metrics; got.Enabled {
		t.Fatal("metrics enabled by default")
	}
}

func TestStartSessionViewEnabledServesWithControlToken(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := startSessionView(ctx, metrics.NewHub("7.7.7", nil), config.MetricsConfig{Enabled: true, Listen: addr}, "ctrl-token")
	if srv == nil {
		t.Fatal("enabled metrics did not start")
	}
	t.Cleanup(func() { _ = srv.Close() })

	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer ctrl-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(buf.String(), `unarr_info{version="7.7.7"} 1`) {
		t.Fatalf("status %d body %q", resp.StatusCode, buf.String())
	}
}

func TestStartSessionViewRefusesUnprotectedEndpoints(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := metrics.NewHub("v", nil)

	// No token of any kind.
	if srv := startSessionView(ctx, hub, config.MetricsConfig{Enabled: true, Listen: freeAddr(t)}, ""); srv != nil {
		t.Fatal("started without any token")
	}
	// LAN bind relying on the ephemeral control token.
	if srv := startSessionView(ctx, hub, config.MetricsConfig{Enabled: true, Listen: "0.0.0.0:0"}, "ctrl"); srv != nil {
		t.Fatal("started a non-loopback bind without its own token")
	}
}

func TestHLSReadingMapsHealth(t *testing.T) {
	now := time.Now()
	base := engine.HLSInfo{ID: "s1", FileName: "Movie.mkv", LastTouch: now, StartedAt: now.Add(-time.Minute), ServedBytes: 99}

	ok := base
	ok.Stats = engine.TranscodeStats{SpeedX: 1.4, Samples: 5}
	if r := hlsReading(ok, now); r.Impaired || r.SpeedX == nil || *r.SpeedX != 1.4 || r.Bytes != 99 || r.State != "active" {
		t.Fatalf("healthy encode = %+v", r)
	}

	slow := base
	slow.Stats = engine.TranscodeStats{SpeedX: 0.4, Samples: 5}
	if r := hlsReading(slow, now); !r.Impaired || r.Reason != "transcode" {
		t.Fatalf("struggling encode = %+v", r)
	}

	// "marginal" still plays: not an impairment.
	marginal := base
	marginal.Stats = engine.TranscodeStats{SpeedX: 0.85, Samples: 5}
	if r := hlsReading(marginal, now); r.Impaired {
		t.Fatalf("marginal encode flagged: %+v", r)
	}

	inputBound := base
	inputBound.Stats = engine.TranscodeStats{SpeedX: 0.9, Samples: 5, InputBound: true}
	if r := hlsReading(inputBound, now); !r.Impaired || r.Reason != "input_bound" {
		t.Fatalf("input-bound = %+v", r)
	}

	cached := slow
	cached.Stats.FromCache = true
	if r := hlsReading(cached, now); r.Impaired || r.SpeedX != nil {
		t.Fatalf("cache replay has no live encode to judge: %+v", r)
	}

	idle := slow
	idle.LastTouch = now.Add(-5 * time.Minute)
	if r := hlsReading(idle, now); r.Impaired || r.State != "idle" {
		t.Fatalf("idle session = %+v", r)
	}

	failed := base
	failed.Failed = true
	if r := hlsReading(failed, now); !r.Impaired || r.Reason != "encoder_failed" {
		t.Fatalf("failed = %+v", r)
	}
}

// The endpoint is bound once at startup, so a reload that changed [metrics]
// must say it needs a restart instead of ignoring it in silence.
func TestReportMetricsReload(t *testing.T) {
	var sink bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&sink)
	t.Cleanup(func() { log.SetOutput(prev) })

	started := config.MetricsConfig{Enabled: true, Token: "s3cret"}
	reportMetricsReload(started, started)
	if sink.Len() != 0 {
		t.Fatalf("unchanged [metrics] logged %q", sink.String())
	}
	reportMetricsReload(started, config.MetricsConfig{Enabled: true, Token: "other"})
	out := sink.String()
	if !strings.Contains(out, "[reload] [metrics] changed - RESTART REQUIRED") {
		t.Fatalf("changed [metrics] logged %q", out)
	}
	if strings.Contains(out, "s3cret") || strings.Contains(out, "other") {
		t.Fatalf("the token must not reach the log: %q", out)
	}
}

func TestMountReadingsEmptyWhenIdle(t *testing.T) {
	if got := mountReadings(); len(got) != 0 {
		t.Fatalf("idle mount reported as a session: %+v", got)
	}
}

func TestPrintSessions(t *testing.T) {
	speed := 1.01
	var buf bytes.Buffer
	printSessions(&buf, metrics.Snapshot{
		Agent: metrics.Agent{Version: "1.0.0", UptimeSec: 3600},
		Sessions: []metrics.Session{
			{ID: "abcdef123456", Kind: metrics.KindHLS, Title: "Movie", State: "active", Health: metrics.HealthStalled, SpeedX: &speed, Stall: metrics.Stall{Count: 2}},
			{ID: "dl-1", Kind: metrics.KindDownload, Title: "Show", State: "downloading", Health: metrics.HealthOK, RateBps: 2_000_000, Bytes: 5_000_000},
		},
		Summary: metrics.Summary{VPN: metrics.VPN{Enabled: true, Required: true}, DownloadsByState: map[string]int{"paused": 1}},
	})
	out := buf.String()
	for _, want := range []string{"hls", "download", "1.01x", "2.0 MB/s", "stalled", "DOWN (kill-switch", "agent 1.0.0"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	buf.Reset()
	printSessions(&buf, metrics.Snapshot{})
	if !strings.Contains(buf.String(), "No active sessions") {
		t.Errorf("empty view: %q", buf.String())
	}
}

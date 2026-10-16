package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMetricsOffByDefaultAndLoopback(t *testing.T) {
	m := Default().Metrics
	if m.Enabled {
		t.Fatal("metrics must be opt-in")
	}
	if !m.Loopback() || m.Address() != "127.0.0.1:9877" {
		t.Fatalf("default bind = %s (loopback=%v)", m.Address(), m.Loopback())
	}
}

func TestMetricsValidate(t *testing.T) {
	cases := []struct {
		name string
		m    MetricsConfig
		ok   bool
	}{
		{"disabled ignores junk", MetricsConfig{Listen: "not-an-address"}, true},
		{"default loopback, no token", MetricsConfig{Enabled: true}, true},
		{"explicit loopback", MetricsConfig{Enabled: true, Listen: "127.0.0.1:2112"}, true},
		{"ipv6 loopback", MetricsConfig{Enabled: true, Listen: "[::1]:2112"}, true},
		{"lan needs a token", MetricsConfig{Enabled: true, Listen: "0.0.0.0:9877"}, false},
		{"lan with token", MetricsConfig{Enabled: true, Listen: "0.0.0.0:9877", Token: "t"}, true},
		{"hostname rejected", MetricsConfig{Enabled: true, Listen: "localhost:9877"}, false},
		{"missing port", MetricsConfig{Enabled: true, Listen: "127.0.0.1"}, false},
		{"bad port", MetricsConfig{Enabled: true, Listen: "127.0.0.1:99999"}, false},
	}
	for _, c := range cases {
		if err := c.m.Validate(); (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want ok=%v", c.name, err, c.ok)
		}
	}
}

func TestMetricsRoundTripAndKnownKeys(t *testing.T) {
	f := filepath.Join(t.TempDir(), "config.toml")
	in := Default()
	in.Metrics = MetricsConfig{Enabled: true, Listen: "127.0.0.1:2112", Token: "abc"}
	if err := Save(in, f); err != nil {
		t.Fatal(err)
	}
	out, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if out.Metrics != in.Metrics {
		t.Fatalf("round trip: %+v", out.Metrics)
	}
	if len(out.UnknownKeys()) != 0 {
		t.Fatalf("unknown keys: %v", out.UnknownKeys())
	}
	if issues := out.ValueIssues(); len(issues) != 0 {
		t.Fatalf("issues: %v", issues)
	}
}

// Publishing the session view to the web is on by default and independent of
// the Prometheus switch: a config predating the key, or one that only turns
// Prometheus on or off, still publishes. Only an explicit false turns it off.
func TestMetricsPublishDefaultsOn(t *testing.T) {
	if !Default().Metrics.Publish {
		t.Fatal("Default() must publish")
	}
	cases := map[string]bool{
		"":                             true,
		"[metrics]\nenabled = true\n":  true,
		"[metrics]\nenabled = false\n": true,
		"[metrics]\npublish = false\n": false,
		"[metrics]\npublish = true\n":  true,
		"[metrics]\nenabled = true\npublish = false\n": false,
	}
	for body, want := range cases {
		f := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(f)
		if err != nil {
			t.Fatal(err)
		}
		if c.Metrics.Publish != want {
			t.Errorf("%q: publish = %v, want %v", body, c.Metrics.Publish, want)
		}
		if len(c.UnknownKeys()) != 0 {
			t.Errorf("%q: unknown keys %v", body, c.UnknownKeys())
		}
	}
}

func TestMetricsValueIssueSurfaces(t *testing.T) {
	c := Default()
	c.Metrics = MetricsConfig{Enabled: true, Listen: "0.0.0.0:9877"}
	var found bool
	for _, i := range c.ValueIssues() {
		if i.Key == "metrics" {
			found = true
		}
	}
	if !found {
		t.Fatal("a LAN bind without a token must be reported by `unarr config check`")
	}
}

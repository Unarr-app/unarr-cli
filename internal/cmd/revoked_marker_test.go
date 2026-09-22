package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The marker lives next to config.toml: in a container that is the /config
// volume, the one thing that survives restarts and image updates.
func TestRevokedMarkerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UNARR_CONFIG_DIR", dir)

	if readRevokedMarker() != nil {
		t.Fatal("a fresh config dir reports a delete that never happened")
	}
	writeRevokedMarker("tombstoned-id")
	m := readRevokedMarker()
	if m == nil {
		t.Fatal("recorded delete was not read back")
	}
	if m.AgentID != "tombstoned-id" || m.At.IsZero() {
		t.Errorf("marker = %+v, want the tombstoned id and a timestamp", m)
	}
	if got := filepath.Dir(revokedMarkerPath()); got != dir {
		t.Errorf("marker written to %s, want the config dir %s", got, dir)
	}
	clearRevokedMarker()
	if readRevokedMarker() != nil {
		t.Error("cleared marker still reported")
	}
}

// Garbage on disk must not lock a machine out: an unreadable marker is no
// marker.
func TestRevokedMarkerIgnoresGarbage(t *testing.T) {
	t.Setenv("UNARR_CONFIG_DIR", t.TempDir())
	if err := os.WriteFile(revokedMarkerPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if readRevokedMarker() != nil {
		t.Error("unparseable marker treated as a recorded delete")
	}
}

func TestReconnectRequestedTruthySet(t *testing.T) {
	for v, want := range map[string]bool{"1": true, "true": true, "YES": true, "0": false, "": false, "no": false} {
		t.Setenv("UNARR_RECONNECT", v)
		if got := reconnectRequested(); got != want {
			t.Errorf("UNARR_RECONNECT=%q → %v, want %v", v, got, want)
		}
	}
}

// The refusal has to name every way back in, in the terms of where the agent
// runs: environment variables for a container, commands for a shell.
func TestRevokedRefusalNamesTheWayBack(t *testing.T) {
	m := &revokedMarker{At: time.Date(2026, 9, 22, 14, 55, 0, 0, time.Local)}

	t.Setenv("UNARR_DOCKER", "1")
	msg := revokedRefusal(m, "https://unarr.example.net").Error()
	for _, want := range []string{"2026-09-22 14:55", "UNARR_RECONNECT=1", "UNARR_AUTHKEY", "https://unarr.example.net/profile?tab=agents", "stop the container"} {
		if !strings.Contains(msg, want) {
			t.Errorf("docker refusal %q should mention %q", msg, want)
		}
	}
	if strings.Contains(msg, "unarr login") {
		t.Errorf("docker refusal %q suggests a command a container has no shell for", msg)
	}

	t.Setenv("UNARR_DOCKER", "0")
	if _, err := os.Stat("/.dockerenv"); err == nil {
		t.Skip("running inside a container; the host branch cannot be observed")
	}
	msg = revokedRefusal(m, "").Error()
	for _, want := range []string{"unarr login", "UNARR_RECONNECT=1 unarr up"} {
		if !strings.Contains(msg, want) {
			t.Errorf("host refusal %q should mention %q", msg, want)
		}
	}
}

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

func loadFrom(t *testing.T, body string) (config.Config, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, p
}

// A hand-written opt-out must stick after the daemon's first boot: the marker is
// persisted, and the next Load leaves the user's `false` alone.
func TestPersistUsenetStreamingMarkerMakesOptOutStick(t *testing.T) {
	cfg, path := loadFrom(t, "[downloads]\nusenet_streaming = false\n")
	if !cfg.Download.UsenetStreaming {
		t.Fatal("legacy false must migrate to true on the first Load")
	}
	persistUsenetStreamingMarker(cfg, path)

	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "usenet_streaming_default_applied = true") {
		t.Fatalf("marker not persisted:\n%s", raw)
	}
	// The user now opts out by editing the file.
	edited := strings.Replace(string(raw), "usenet_streaming = true", "usenet_streaming = false", 1)
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if again.Download.UsenetStreaming {
		t.Error("opt-out was re-migrated after the marker was persisted")
	}
	if again.UsenetMarkerPending() {
		t.Error("nothing left to persist once the marker is on disk")
	}
}

// Nothing pending (marker already on disk) → the file is not rewritten at all.
func TestPersistUsenetStreamingMarkerNoopWhenAlreadyApplied(t *testing.T) {
	body := "# my comment\n[downloads]\nusenet_streaming = false\nusenet_streaming_default_applied = true\n"
	cfg, path := loadFrom(t, body)
	persistUsenetStreamingMarker(cfg, path)
	if raw, _ := os.ReadFile(path); string(raw) != body {
		t.Errorf("file rewritten although nothing was pending:\n%s", raw)
	}
}

// A config that cannot be written (read-only mount) must not panic nor stop the
// daemon; the in-memory migration still applies.
func TestPersistUsenetStreamingMarkerReadOnlyIsBestEffort(t *testing.T) {
	cfg, path := loadFrom(t, "[downloads]\nusenet_streaming = false\n")
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skip("cannot make dir read-only here")
	}
	defer os.Chmod(dir, 0o700)
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	persistUsenetStreamingMarker(cfg, path) // must simply return
	if !cfg.Download.UsenetStreaming {
		t.Error("in-memory migration lost")
	}
}

// With --config the daemon must write the file it READ, never the default path.
func TestPersistUsenetStreamingMarkerWritesGivenPath(t *testing.T) {
	cfg, path := loadFrom(t, "[downloads]\nusenet_streaming = false\n")
	other := filepath.Join(t.TempDir(), "default.toml")
	persistUsenetStreamingMarker(cfg, path)
	if _, err := os.Stat(other); err == nil {
		t.Error("an unrelated path was written")
	}
	if raw, _ := os.ReadFile(path); !strings.Contains(string(raw), "usenet_streaming_default_applied") {
		t.Error("given path not updated")
	}
}

package service

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The failure this exists for: the tray stopped the daemon by killing its PID
// while systemd owned it with Restart=always, so "Pause" was undone by the
// supervisor ~10s later. Everything hangs off Respawns() answering truthfully
// for an installed unit.
func TestRespawnsDetectsAnInstalledSupervisor(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no respawning supervisor on this OS")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	if Respawns() {
		t.Fatal("no service installed yet, want false")
	}

	path := SystemdUnitPathIn(home)
	if runtime.GOOS == "darwin" {
		path = PlistPath(home)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("unit"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !Respawns() {
		t.Fatalf("service installed at %s, want true", path)
	}
}

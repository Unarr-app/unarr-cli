package config

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestMountDefaultsAndRoundTrip(t *testing.T) {
	if Default().Mount.Enabled {
		t.Fatal("mount enabled by default")
	}
	f := filepath.Join(t.TempDir(), "config.toml")
	initial := Default()
	directory := filepath.Join(t.TempDir(), "unarr-media")
	initial.Mount = MountConfig{Enabled: true, Directory: directory, RefreshInterval: "30s"}
	if err := Save(initial, f); err != nil {
		t.Fatal(err)
	}
	c, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Mount.Enabled {
		t.Fatal(c.Mount)
	}
	if err = c.Mount.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(c.UnknownKeys()) != 0 {
		t.Fatal(c.UnknownKeys())
	}
	if err = Save(c, f); err != nil {
		t.Fatal(err)
	}
	got, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mount.RefreshInterval != "30s" || got.Mount.Directory != directory {
		t.Fatal("lost mount settings")
	}
}

func TestMountWindowsDriveGrammar(t *testing.T) {
	for _, dir := range []string{"A:", "Z:", "a:", "z:"} {
		if !IsWindowsMountDrive(dir) {
			t.Fatal("bare drive rejected", dir)
		}
		m := MountConfig{Enabled: true, Directory: dir}
		if (m.Validate() == nil) != (runtime.GOOS == "windows") {
			t.Fatal("platform drive validation mismatch", dir)
		}
	}
	for _, dir := range []string{"X:media", "X:.", "1:", "é:", "[:", "X:\\media", ""} {
		if IsWindowsMountDrive(dir) {
			t.Fatal("invalid bare drive accepted", dir)
		}
	}
}

func TestMountNZBDirectoryUsesManagedDefaultAndExplicitOverride(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	m := MountConfig{}
	if got, want := m.NZBDirectory(configPath), filepath.Join(filepath.Dir(configPath), "mount-nzbs"); got != want {
		t.Fatalf("NZBDirectory() = %q, want %q", got, want)
	}
	m.NZBDir = filepath.Join(t.TempDir(), "custom")
	if got := m.NZBDirectory(configPath); got != m.NZBDir {
		t.Fatalf("NZBDirectory() ignored override: %q", got)
	}
}

func TestMountValidation(t *testing.T) {
	base := MountConfig{Enabled: true}
	for _, address := range []string{"0.0.0.0:11820", "192.168.1.1:11820", "example.com:11820", "127.0.0.1:70000"} {
		m := base
		m.Listen = address
		if m.Validate() == nil {
			t.Fatalf("accepted %q", address)
		}
	}
	for _, address := range []string{"127.0.0.1:11820", "[::1]:11820"} {
		m := base
		m.Listen = address
		if err := m.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	m := base
	m.RefreshInterval = "-1s"
	if m.Validate() == nil {
		t.Fatal("negative interval")
	}
	m = base
	m.Directory = "relative"
	if m.Validate() == nil {
		t.Fatal("relative mount directory")
	}
	m = base
	m.Enabled = false
	m.Listen = "invalid"
	if err := m.Validate(); err != nil {
		t.Fatal("disabled settings not inert", err)
	}
}

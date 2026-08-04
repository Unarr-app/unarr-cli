package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A reinstall must not be able to destroy a working service definition. The
// previous implementation opened the target with os.Create, which truncates
// before anything is known to render — a failure there left an empty unit that
// systemd refuses to load, and the daemon did not come back.
func TestWriteServiceFileKeepsThePreviousDefinitionWhenRenderingFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unarr.service")
	const previous = "[Unit]\nDescription=the install that already works\n"
	if err := os.WriteFile(path, []byte(previous), 0o644); err != nil {
		t.Fatal(err)
	}

	// A template that parses and then fails at execution time: serviceData has
	// no such field.
	err := writeServiceFile(path, "{{ .NoSuchField }}", serviceData{BinPath: "/usr/bin/unarr"})
	if err == nil {
		t.Fatal("writeServiceFile succeeded with an unrenderable template")
	}

	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("the previous service file is gone: %v", readErr)
	}
	if string(got) != previous {
		t.Errorf("the previous service file was damaged:\n%s", got)
	}
}

func TestWriteServiceFileLeavesNoTempFileBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unarr.service")

	if err := writeServiceFile(path, "{{ .NoSuchField }}", serviceData{}); err == nil {
		t.Fatal("writeServiceFile succeeded with an unrenderable template")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".unarr-service-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

// The supervisor reads this file as a different user in the system-unit case,
// so it cannot keep os.CreateTemp's 0600.
func TestWriteServiceFileIsReadableByTheSupervisor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unarr.service")

	if err := writeServiceFile(path, systemdTemplate, serviceData{
		BinPath: "/usr/bin/unarr", User: "u", Home: "/home/u", LogDir: dir,
	}); err != nil {
		t.Fatalf("writeServiceFile() = %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode = %o, want 644 — 600 would leave the unit unreadable", perm)
	}
}

// A rewrite over a valid file must produce the new content, not append to or
// mix with the old one.
func TestWriteServiceFileReplacesTheContentWholesale(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unarr.service")
	if err := os.WriteFile(path, []byte(strings.Repeat("X", 4096)), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := writeServiceFile(path, systemdTemplate, serviceData{
		BinPath: "/usr/bin/unarr", User: "u", Home: "/home/u", LogDir: dir,
	}); err != nil {
		t.Fatalf("writeServiceFile() = %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "XXXX") {
		t.Error("the old content survived alongside the new definition")
	}
	if !strings.Contains(string(got), "/usr/bin/unarr") {
		t.Errorf("the new definition was not written:\n%s", got)
	}
}

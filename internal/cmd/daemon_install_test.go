package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/service"
	"github.com/fatih/color"
)

// A unit/plist that survives a FAILED install is not inert: service.Respawns()
// treats the file's existence as "a supervisor owns the daemon", so `unarr stop`
// delegates to systemctl/launchctl on a box that has no such supervisor —
// erroring, or claiming "✓ Stopped" while a detached daemon keeps running.
//
// Both tests force the failure by emptying PATH, so the LookPath probe for the
// service manager fails without any real service being touched.
func TestInstallSystemdRemovesUnitFileOnFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PATH", t.TempDir())

	data := serviceData{
		BinPath: filepath.Join(home, "unarr"),
		User:    "tester",
		Home:    home,
		LogDir:  filepath.Join(home, "logs"),
	}
	if err := installSystemd(data, color.New(color.FgGreen)); err == nil {
		t.Fatal("installSystemd() = nil, want an error when systemctl is absent")
	}

	path := service.SystemdUnitPathIn(home)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unit file %s survived a failed install (stat err: %v)", path, err)
	}
}

func TestInstallLaunchdRemovesPlistOnFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PATH", t.TempDir())

	data := serviceData{
		BinPath: filepath.Join(home, "unarr"),
		User:    "tester",
		Home:    home,
		LogDir:  filepath.Join(home, "logs"),
	}
	if err := installLaunchd(data, color.New(color.FgGreen)); err == nil {
		t.Fatal("installLaunchd() = nil, want an error when launchctl is absent")
	}

	path := service.PlistPath(home)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("plist %s survived a failed install (stat err: %v)", path, err)
	}
}

// The install writes exactly the path detection reads, and the rendered unit
// keeps the fields the daemon depends on (absolute ExecStart + HOME, which is
// what makes a user unit find its own config).
func TestWriteServiceFileRendersSystemdUnit(t *testing.T) {
	home := t.TempDir()
	path := service.SystemdUnitPathIn(home)
	data := serviceData{BinPath: "/usr/local/bin/unarr", User: "tester", Home: home}

	if err := writeServiceFile(path, systemdTemplate, data); err != nil {
		t.Fatalf("writeServiceFile() = %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("unit file not written where detection looks: %v", err)
	}
	for _, want := range []string{
		"ExecStart=/usr/local/bin/unarr start",
		"Environment=HOME=" + home,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("unit file missing %q:\n%s", want, body)
		}
	}
}

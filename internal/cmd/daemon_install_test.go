package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/service"
	"github.com/fatih/color"
)

// The service the installer writes and the log every reader opens must be the
// same file. LogDir was hardcoded to the Linux XDG path, so on macOS launchd
// wrote ~/.local/share/unarr/unarr.log while `unarr logs`, the janitor and
// `clean` all resolved ~/Library/Application Support/unarr — "no daemon log yet"
// with the daemon running, and a log nothing kept inside its budget.
func TestResolveServiceDataLogsWhereTheReadersLook(t *testing.T) {
	withDataDir(t)

	data, err := resolveServiceData()
	if err != nil {
		t.Fatalf("resolveServiceData() = %v", err)
	}
	if data.LogDir != config.DataDir() {
		t.Errorf("LogDir = %s, want config.DataDir() = %s", data.LogDir, config.DataDir())
	}
}

// launchd is the only supervisor that could send stdout and stderr to different
// files, and the daemon logs through log.Printf — stderr. Pointing the two keys
// at different files left `unarr logs` reading a unarr.log with nothing but the
// start banner in it.
func TestLaunchdPlistSendsBothStreamsToTheSameLog(t *testing.T) {
	body := renderLaunchdPlist(t)

	want := "<string>" + launchdTestLogDir + "/" + bootLogFileName + "</string>"
	if got := strings.Count(body, want); got != 2 {
		t.Errorf("plist points %d of its 2 output paths at the boot log:\n%s", got, body)
	}
}

// The daemon has to OWN unarr.log for rename rotation to work, and launchd has
// to hold a DIFFERENT file — otherwise the only rotation that shrinks a live
// file is unavailable and the log falls back to copy-truncate under a holder.
func TestLaunchdPlistGivesTheDaemonItsOwnLog(t *testing.T) {
	body := renderLaunchdPlist(t)
	ownedLog := launchdTestLogDir + "/" + logFileName

	for _, want := range []string{
		"<string>--log-file</string>",
		"<string>" + ownedLog + "</string>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("plist does not hand the daemon its own log (missing %s):\n%s", want, body)
		}
	}
	// The supervisor's redirect must not land on the file the daemon owns: two
	// holders on one path is precisely what rename rotation cannot survive.
	for _, key := range []string{"StandardOutPath", "StandardErrorPath"} {
		block := "<key>" + key + "</key>\n  <string>" + ownedLog + "</string>"
		if strings.Contains(body, block) {
			t.Errorf("%s points at the log the daemon owns:\n%s", key, body)
		}
	}
}

const launchdTestLogDir = "/tmp/unarr-plist-test/logs"

// renderLaunchdPlist writes the plist exactly where detection looks and returns
// its body.
func renderLaunchdPlist(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	path := service.PlistPath(home)

	if err := writeServiceFile(path, launchdTemplate, serviceData{
		BinPath: "/usr/local/bin/unarr", User: "tester", Home: home, LogDir: launchdTestLogDir,
	}); err != nil {
		t.Fatalf("writeServiceFile() = %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("plist not written where detection looks: %v", err)
	}
	return string(body)
}

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

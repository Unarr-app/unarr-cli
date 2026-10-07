package cmd

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunningAsSystemdUnit(t *testing.T) {
	cases := []struct {
		name   string
		cgroup string
		want   bool
	}{
		{"the installed user unit (cgroup v2)",
			"0::/user.slice/user-1000.slice/user@1000.service/app.slice/unarr.service\n", true},
		// `systemctl --user` cannot stop it, and might stop the user's own unit.
		{"a system unit of the same name", "0::/system.slice/unarr.service\n", false},
		{"cgroup v1, unit in the name=systemd hierarchy",
			"12:pids:/user.slice\n1:name=systemd:/user.slice/user-1000.slice/user@1000.service/unarr.service\n", true},
		{"a foreground start in a terminal",
			"0::/user.slice/user-1000.slice/user@1000.service/app.slice/app-orca-438022.scope\n", false},
		{"another unit whose name ends like ours",
			"0::/user.slice/user-1000.slice/user@1000.service/app.slice/my-unarr.service\n", false},
		{"a container", "0::/\n", false},
		{"no /proc (macOS, Windows)", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := runningAsSystemdUnit(c.cgroup); got != c.want {
				t.Errorf("runningAsSystemdUnit(%q) = %v, want %v", c.cgroup, got, c.want)
			}
		})
	}
}

func TestShimChain(t *testing.T) {
	procs := map[uint32]procEntry{
		100: {parent: 4, exe: "svchost.exe"},
		200: {parent: 100, exe: "WScript.exe"}, // the task's launcher shim
		300: {parent: 200, exe: "cmd.exe"},
		400: {parent: 300, exe: "unarr.exe"}, // daemon under the shim
		500: {parent: 1, exe: "explorer.exe"},
		600: {parent: 500, exe: "WindowsTerminal.exe"},
		700: {parent: 600, exe: "cmd.exe"},
		800: {parent: 700, exe: "unarr.exe"}, // `unarr start` typed in cmd
		900: {parent: 500, exe: "unarr-desktop.exe"},
		910: {parent: 900, exe: "unarr.exe"},   // tray's detached start
		920: {parent: 12345, exe: "unarr.exe"}, // parent already gone
	}
	for self, want := range map[uint32]bool{400: true, 800: false, 910: false, 920: false, 999: false} {
		if got := shimChain(procs, self); got != want {
			t.Errorf("shimChain(pid %d) = %v, want %v", self, got, want)
		}
	}
}

func TestNeedsSignInSurvivesWrapping(t *testing.T) {
	err := needsSignIn("no API key configured — %s", "run unarr login")
	if !isNeedsSignIn(err) || !isNeedsSignIn(fmt.Errorf("start: %w", err)) {
		t.Fatal("a wrapped needs-sign-in error must still park the service")
	}
	if err.Error() != "no API key configured — run unarr login" {
		t.Errorf("message = %q", err.Error())
	}
	if isNeedsSignIn(fmt.Errorf("no API key configured")) {
		t.Error("a plain error is a failure, not a sign-in wait")
	}
}

// The unit must not restart on the sign-in exit even when the stop request
// cannot reach the user manager, and must not read "failed" for it either; the
// numbers have to agree.
func TestSystemdUnitStopsOnSignInExit(t *testing.T) {
	for _, key := range []string{"SuccessExitStatus", "RestartPreventExitStatus"} {
		want := fmt.Sprintf("%s=%d\n", key, exitNeedsSignIn)
		if !strings.Contains(systemdTemplate, want) {
			t.Errorf("systemd template lacks %q", want)
		}
	}
}

// The marker belongs to the config in use: a dev agent's --config must neither
// spend nor find the installed service's marker.
func TestParkedMarkerFollowsTheConfigInUse(t *testing.T) {
	defaultDir, devDir := t.TempDir(), t.TempDir()
	t.Setenv("UNARR_CONFIG_DIR", defaultDir)
	prev := cfgFile
	t.Cleanup(func() { cfgFile = prev })

	cfgFile = ""
	if got := filepath.Dir(parkedMarkerPath()); got != defaultDir {
		t.Errorf("default config: marker in %s, want %s", got, defaultDir)
	}
	cfgFile = filepath.Join(devDir, "config.toml")
	if got := filepath.Dir(parkedMarkerPath()); got != devDir {
		t.Errorf("--config: marker in %s, want %s", got, devDir)
	}
}

// Only a service that parked itself is resumed: without the marker a sign-in
// leaves the service alone, whatever state it is in.
func TestResumeWithoutMarkerDoesNothing(t *testing.T) {
	t.Setenv("UNARR_CONFIG_DIR", t.TempDir())
	prev := cfgFile
	t.Cleanup(func() { cfgFile = prev })
	cfgFile = ""

	out := captureStdout(t, resumeInstalledService)
	if out != "" {
		t.Errorf("resume without a marker printed %q, want nothing", out)
	}
}

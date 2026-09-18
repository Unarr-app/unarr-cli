package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Opt-in: this exercises the REAL launchd, with unique labels and temporary
// plists. It never touches the user's unarr service, config, or credentials.
func realLaunchdAgent(t *testing.T, mode string) *launchdAgent {
	t.Helper()
	if os.Getenv("UNARR_LAUNCHD_E2E") != "1" {
		t.Skip("set UNARR_LAUNCHD_E2E=1 to exercise real launchd")
	}
	home := t.TempDir()
	a, err := newLaunchdAgent(home)
	if err != nil {
		t.Fatal(err)
	}
	a.label = fmt.Sprintf("com.torrentclaw.unarr.test.%d.%d", os.Getpid(), time.Now().UnixNano())
	a.path = filepath.Join(home, a.label+".plist")
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tmpl := `<plist version="1.0"><dict>
<key>Label</key><string>` + a.label + `</string>
<key>ProgramArguments</key><array><string>{{.BinPath | html}}</string><string>-test.run=^TestLaunchdProcessHelper$</string></array>
<key>EnvironmentVariables</key><dict><key>UNARR_LAUNCHD_HELPER</key><string>` + mode + `</string></dict>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
<key>LimitLoadToSessionType</key><array><string>Aqua</string><string>Background</string></array>
<key>ThrottleInterval</key><integer>1</integer>
<key>StandardOutPath</key><string>{{.LogDir | html}}/boot.log</string>
<key>StandardErrorPath</key><string>{{.LogDir | html}}/boot.log</string>
</dict></plist>`
	if err := writeServiceFile(a.path, tmpl, serviceData{BinPath: bin, LogDir: home}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.stop(); err != nil {
			t.Error(err)
		}
		if err := a.command("enable", a.target()); err != nil {
			t.Error(err)
		}
	})
	return a
}

func TestLaunchdProcessHelper(t *testing.T) {
	mode := os.Getenv("UNARR_LAUNCHD_HELPER")
	if mode == "" {
		return
	}
	if mode == "crash" {
		os.Exit(42)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	<-sig
	if mode == "slow" {
		time.Sleep(2 * time.Second)
	}
	os.Exit(0)
}

func TestLaunchdRealLifecycle(t *testing.T) {
	a := realLaunchdAgent(t, "slow")
	if err := a.start(); err != nil {
		t.Fatal(err)
	}
	out, _, _ := a.status()
	pid := launchdPID(out)
	// Reproduce John's exact symptom with the old command. On macOS 26 it
	// reports error 5 on stderr while returning success to Go's exec.Run.
	legacyOut, legacyErr := a.run("load", a.path)
	t.Logf("legacy load: err=%v output=%s", legacyErr, legacyOut)
	if !strings.Contains(legacyOut, "Load failed: 5") {
		t.Fatal("legacy duplicate-load reproduction did not produce error 5")
	}
	for range 3 {
		if err := a.start(); err != nil {
			t.Fatal(err)
		}
		out, _, _ := a.status()
		if launchdPID(out) != pid {
			t.Fatal("repeated Start replaced a healthy daemon")
		}
	}
	for range 3 {
		start := time.Now()
		if err := a.stop(); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed < 2*time.Second {
			t.Fatalf("returned before slow shutdown: %s", elapsed)
		}
		if err := a.stop(); err != nil {
			t.Fatal("repeated Stop failed", err)
		}
		if err := a.start(); err != nil {
			t.Fatal("reload after slow shutdown", err)
		}
	}
	// A missing file must not prevent stopping or starting an existing job.
	if err := os.Remove(a.path); err != nil {
		t.Fatal(err)
	}
	if err := a.start(); err != nil {
		t.Fatal("orphaned registration was not recoverable", err)
	}
	if err := a.stop(); err != nil {
		t.Fatal(err)
	}
	if err := a.start(); err == nil || !strings.Contains(err.Error(), "daemon install") {
		t.Fatalf("missing definition should explain repair: %v", err)
	}
}

func TestLaunchdRealDisabledAndCrashRecovery(t *testing.T) {
	a := realLaunchdAgent(t, "normal")
	if err := a.command("disable", a.target()); err != nil {
		t.Fatal(err)
	}
	out, err := a.run("load", a.path)
	t.Logf("disabled legacy load: err=%v output=%s", err, out)
	if !strings.Contains(out, "Load failed: 5") {
		t.Fatal("disabled reproduction did not produce error 5")
	}
	if err := a.start(); err != nil {
		t.Fatal("could not recover disabled service", err)
	}
	out, _, _ = a.status()
	oldPID := launchdPID(out)
	if err := a.command("kill", "SIGKILL", a.target()); err != nil {
		t.Fatal(err)
	}
	// KeepAlive must recover a killed process without deleting the plist.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		out, _, err = a.status()
		if err != nil {
			t.Fatal(err)
		}
		pid := launchdPID(out)
		if pid != 0 && pid != oldPID {
			if err := a.waitRunning(); err != nil {
				t.Fatal(err)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("KeepAlive did not recover the killed daemon")
}

func TestLaunchdRealRejectsCrashLoop(t *testing.T) {
	a := realLaunchdAgent(t, "crash")
	if err := a.start(); err == nil || !strings.Contains(err.Error(), "did not stay running") {
		t.Fatalf("reported success for a process exiting 42: %v", err)
	}
	out, loaded, err := a.status()
	if err != nil || !loaded || !strings.Contains(out, "last exit code = 42") {
		t.Fatalf("missing crash diagnosis: loaded=%v err=%v\n%s", loaded, err, out)
	}
}

func TestLaunchdRealPreservesUserDomain(t *testing.T) {
	a := realLaunchdAgent(t, "normal")
	a.domain = "user/" + strconv.Itoa(os.Getuid())
	if err := a.start(); err != nil {
		t.Fatal(err)
	}
	b := *a
	b.domain = ""
	if err := b.findDomain(); err != nil {
		t.Fatal(err)
	}
	if b.domain != a.domain {
		t.Fatalf("existing user-domain agent moved to %s", b.domain)
	}
	if err := b.start(); err != nil {
		t.Fatal(err)
	}
}

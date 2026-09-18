package cmd

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/service"
	"github.com/fatih/color"
)

// Requires a disposable macOS account with neither supported daemon installed.
// The binary talks ONLY to the loopback API fixture and uses a temporary HOME.
func TestLaunchdRealCLIInstall(t *testing.T) {
	bin := os.Getenv("UNARR_LAUNCHD_CLI")
	if bin == "" || os.Getenv("UNARR_LAUNCHD_E2E") != "1" {
		t.Skip("set UNARR_LAUNCHD_E2E=1 and UNARR_LAUNCHD_CLI to a built macOS CLI")
	}
	assertNoInstalledLaunchdDaemon(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("UNARR_CONFIG_DIR", filepath.Join(home, "config"))
	oldCfg, oldLoaded, oldFile := appCfg, cfgLoaded, cfgFile
	cfgLoaded, cfgFile = false, config.FilePath()
	t.Cleanup(func() { appCfg, cfgLoaded, cfgFile = oldCfg, oldLoaded, oldFile })
	a, err := newLaunchdAgent(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.stop(); err != nil {
			t.Error(err)
		}
		legacy := *a
		legacy.label = service.LegacyLaunchdLabel
		if err := legacy.stop(); err != nil {
			t.Error(err)
		}
	})
	server := launchdFixtureAPI(t)
	cfg := config.Default()
	cfg.Auth.APIKey, cfg.Auth.APIURL = "local-launchd-fixture", server.URL
	cfg.Auth.Mirrors = nil
	cfg.Agent.ID, cfg.Agent.Name = "launchd-fixture", "Launchd test"
	cfg.Download.Dir = filepath.Join(home, "downloads")
	cfg.Download.PreferredMethods = []string{"debrid"}
	cfg.Download.HTTPSStreamPort, cfg.Download.StreamPort = 0, 0
	cfg.Download.AutoHTTPSUpnp, cfg.Download.EnableUPnP = false, false
	cfg.Download.Transcode.Enabled = false
	cfg.Download.Funnel.Enabled, cfg.Download.VPN.Enabled = false, false
	disabled := false
	cfg.Daemon.AutoUpgrade, cfg.Telemetry.Enabled = &disabled, &disabled
	cfg.Daemon.Downlink = "poll"
	if err := config.Save(cfg, config.FilePath()); err != nil {
		t.Fatal(err)
	}
	// A wrapper is only for the fixture: it carries the private config across
	// the launchd boundary without changing launchd's global environment.
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	wrapper := filepath.Join(home, "unarr fixture & runner")
	body := "#!/bin/sh\nexport HOME=" + quote(home) + "\nexport UNARR_CONFIG_DIR=" + quote(config.Dir()) +
		"\nexport PATH=/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin\nexec " + quote(bin) + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	data := serviceData{BinPath: wrapper, Home: home, LogDir: config.DataDir()}
	for range 2 {
		if err := installLaunchd(data, color.New(color.FgGreen)); err != nil {
			t.Fatalf("install/reinstall: %v\n%s", err, tailLogFile(daemonBootLogPath(), 20))
		}
		waitLaunchdFixtureRegistered(t, 0)
	}
	cli := func(args ...string) {
		t.Helper()
		out, err := exec.Command(bin, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("unarr %v: %v\n%s", args, err, out)
		}
		t.Logf("unarr %v: %s", args, strings.TrimSpace(string(out)))
	}
	pid := agent.ReadState().PID
	cli("daemon", "start")
	if agent.ReadState().PID != pid {
		t.Fatal("CLI Start replaced a healthy daemon")
	}
	if err := a.command("kill", "SIGKILL", a.target()); err != nil {
		t.Fatal(err)
	}
	waitLaunchdFixtureRegistered(t, pid)
	pid = agent.ReadState().PID
	cli("daemon", "restart")
	waitLaunchdFixtureRegistered(t, pid)
	if agent.ReadState().PID == pid {
		t.Fatal("CLI Restart did not replace the daemon")
	}
	cli("daemon", "status")
	cli("daemon", "stop")
	cli("daemon", "stop")
	// Recreate the label and filename from the reported workaround, then prove
	// reinstall retires it before starting the canonical agent.
	legacyPath := service.LegacyPlistPath(home)
	// The filename and Label need not match: older/manual installs can keep
	// the canonical label in the alternate filename. It must remain startable.
	if err := writeServiceFile(legacyPath, launchdTemplate, data); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(a.path); err != nil {
		t.Fatal(err)
	}
	cli("daemon", "start")
	waitLaunchdFixtureRegistered(t, 0)
	pid = agent.ReadState().PID
	cli("daemon", "start")
	if agent.ReadState().PID != pid {
		t.Fatal("alternate filename replaced a healthy process")
	}
	cli("daemon", "stop")
	legacyTemplate := strings.ReplaceAll(launchdTemplate, service.LaunchdLabel, service.LegacyLaunchdLabel)
	if err := writeServiceFile(legacyPath, legacyTemplate, data); err != nil {
		t.Fatal(err)
	}
	legacy := *a
	legacy.label, legacy.path = service.LegacyLaunchdLabel, legacyPath
	cli("daemon", "start")
	waitLaunchdFixtureRegistered(t, 0)
	cli("stop")
	cli("daemon", "start")
	if err := installLaunchd(data, color.New(color.FgGreen)); err != nil {
		t.Fatal("migrate legacy agent", err)
	}
	if _, loaded, err := legacy.status(); err != nil || loaded {
		t.Fatalf("legacy job survived: loaded=%v err=%v", loaded, err)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatal("legacy plist survived", err)
	}
	waitLaunchdFixtureRegistered(t, 0)
	// A real startup error must preserve control of the loaded job and recover
	// after the config is repaired, without deleting a plist or any cache.
	cfg.Auth.APIKey = ""
	if err := config.Save(cfg, config.FilePath()); err != nil {
		t.Fatal(err)
	}
	if err := installLaunchd(data, color.New(color.FgGreen)); err == nil {
		t.Fatal("install reported success with missing credentials")
	}
	if _, err := os.Stat(a.path); err != nil {
		t.Fatal("failed startup deleted the service definition", err)
	}
	if _, loaded, err := a.status(); err != nil || !loaded {
		t.Fatalf("failed startup lost registration: loaded=%v err=%v", loaded, err)
	}
	cfg.Auth.APIKey = "local-launchd-fixture"
	if err := config.Save(cfg, config.FilePath()); err != nil {
		t.Fatal(err)
	}
	cli("daemon", "start")
	waitLaunchdFixtureRegistered(t, 0)
	cli("daemon", "uninstall")
	cli("daemon", "uninstall")
	if _, loaded, err := a.status(); err != nil || loaded {
		t.Fatalf("uninstall left registration: loaded=%v err=%v", loaded, err)
	}
}

func waitLaunchdFixtureRegistered(t *testing.T, oldPID int) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		if st := agent.ReadState(); st != nil && st.PID != oldPID && st.Status == "running" && agent.IsProcessAlive(st.PID) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("daemon did not register with fixture\n%s\n%s", tailLogFile(daemonBootLogPath(), 15), tailLogFile(daemonLogPath(), 15))
}

func launchdFixtureAPI(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/register") {
			fmt.Fprint(w, `{"success":true,"user":{"name":"Fixture","plan":"pro"},"features":{}}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/wake") {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(time.Second):
			}
		}
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(s.Close)
	return s
}

func assertNoInstalledLaunchdDaemon(t *testing.T) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{service.PlistPath(home), service.LegacyPlistPath(home)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("refusing to touch existing install: %s", path)
		}
	}
	for _, domain := range []string{"gui/", "user/"} {
		domain += strconv.Itoa(os.Getuid())
		if out, err := launchctlOutput("print", domain); err != nil {
			var exit interface{ ExitCode() int }
			if errors.As(err, &exit) && exit.ExitCode() == 112 {
				continue // This session domain does not exist (e.g. SSH without GUI).
			}
			t.Fatalf("refusing to probe inaccessible domain %s: %v\n%s", domain, err, out)
		}
		for _, label := range []string{service.LaunchdLabel, service.LegacyLaunchdLabel} {
			a := launchdAgent{domain: domain, label: label, run: launchctlOutput}
			if _, loaded, err := a.status(); err != nil || loaded {
				t.Fatalf("refusing to touch %s: loaded=%v err=%v", a.target(), loaded, err)
			}
		}
	}
}

package cmd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/service"
)

// Use isolated platform paths and restore all process-global CLI config state.
func isolatedMountConfig(t *testing.T) config.Config {
	t.Helper()
	home := t.TempDir()
	stdin, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	previousStdin := os.Stdin
	os.Stdin = stdin
	t.Cleanup(func() { os.Stdin = previousStdin; _ = stdin.Close() })
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", home)
	t.Setenv("LOCALAPPDATA", home)
	for _, key := range []string{"UNARR_CONFIG_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "UNARR_API_KEY", "UNARR_API_URL", "UNARR_DOWNLOAD_DIR", "UNARR_COUNTRY", "UNARR_TELEMETRY"} {
		t.Setenv(key, "")
	}
	previous, loaded, previousFile, previousError := appCfg, cfgLoaded, cfgFile, errCfgLoad
	t.Cleanup(func() { appCfg, cfgLoaded, cfgFile, errCfgLoad = previous, loaded, previousFile, previousError })
	cfg := config.Default()
	cfg.Auth.APIKey, cfg.Agent.ID = "synthetic-key", "synthetic-agent"
	cfg.Download.Dir = t.TempDir()
	cfg.Mount.Enabled = true
	cfgFile, errCfgLoad = "", nil
	appCfg, cfgLoaded = cfg, true
	return cfg
}

func saveMountFixture(t *testing.T, cfg config.Config) []byte {
	t.Helper()
	if err := config.Save(cfg, resolvedConfigPath()); err != nil {
		t.Fatal(err)
	}
	appCfg = cfg
	b, err := os.ReadFile(resolvedConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPersistentMountCustomConfigRejectedBeforeAccess(t *testing.T) {
	cfg := isolatedMountConfig(t)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	cfg.Auth.APIURL = srv.URL
	defaultBefore := saveMountFixture(t, cfg)
	cfgFile = filepath.Join(t.TempDir(), "other.toml")
	before := saveMountFixture(t, cfg)
	cmd := newMountCmd()
	cmd.SetContext(context.Background())
	err := runMountCommand(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "default") || requests.Load() != 0 {
		t.Fatalf("custom config reached authentication: calls=%d error=%v", requests.Load(), err)
	}
	after, _ := os.ReadFile(cfgFile)
	if string(before) != string(after) {
		t.Fatal("custom config was mutated")
	}
	defaultAfter, _ := os.ReadFile(config.FilePath())
	if string(defaultBefore) != string(defaultAfter) {
		t.Fatal("unrelated default service config changed")
	}
}

func TestMountDaemonPreflightBeforeAccess(t *testing.T) {
	cfg := isolatedMountConfig(t)
	cfg.Download.Dir = ""
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	cfg.Auth.APIURL = srv.URL
	before := saveMountFixture(t, cfg)
	cmd := newMountCmd()
	cmd.SetContext(context.Background())
	err := runMountCommand(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "download directory") || requests.Load() != 0 {
		t.Fatalf("missing daemon prerequisite reached authentication: calls=%d error=%v", requests.Load(), err)
	}
	after, _ := os.ReadFile(resolvedConfigPath())
	if string(before) != string(after) {
		t.Fatal("failed preflight changed config")
	}
}

func TestMountRevokedIdentityClearedBeforeSignIn(t *testing.T) {
	for _, status := range []int{410, 403, 401, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			cfg := isolatedMountConfig(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				if status == 403 {
					_, _ = w.Write([]byte(`{"error":"agent_key_mismatch"}`))
				}
			}))
			defer srv.Close()
			cfg.Auth.APIURL = srv.URL
			saveMountFixture(t, cfg)
			err := checkMountAccount(context.Background(), &cfg)
			if err == nil {
				t.Fatal("unexpected access")
			}
			after, loadErr := config.Load(resolvedConfigPath())
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if status == 410 || status == 403 {
				if after.Auth.APIKey != "" || after.Agent.ID != "" || !strings.Contains(err.Error(), "sign-in") {
					t.Fatalf("revoked identity was retained: status=%d error=%v", status, err)
				}
			} else if after.Auth.APIKey != cfg.Auth.APIKey || after.Agent.ID != cfg.Agent.ID {
				t.Fatal("non-revocation cleared the identity")
			}
		})
	}
}

func TestUmountStoppedServiceDoesNotRestart(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("isolated systemctl fixture")
	}
	for _, intent := range []string{"inactive", "parked", "stopped"} {
		t.Run(intent, func(t *testing.T) { verifyStoppedUmount(t, intent) })
	}
}

func verifyStoppedUmount(t *testing.T, intent string) {
	t.Helper()
	cfg := isolatedMountConfig(t)
	saveMountFixture(t, cfg)
	unit := service.UnitPath()
	if err := os.MkdirAll(filepath.Dir(unit), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit, []byte("synthetic unit"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	calls := filepath.Join(t.TempDir(), "calls")
	t.Setenv("MOUNT_TEST_SERVICE_CALLS", calls)
	t.Setenv("PATH", bin)
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$MOUNT_TEST_SERVICE_CALLS\"\ncase \"$*\" in *is-active*) echo inactive; exit 3;; esac\n"
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	park := parkedMarkerPath()
	if intent == "parked" {
		if err := os.WriteFile(park, []byte("parked"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if intent == "stopped" {
		agent.WriteStopIntent()
	}
	c := newUmountCmd()
	if err := c.RunE(c, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(calls)
	if strings.Contains(string(got), "restart") || strings.Contains(string(got), " start ") {
		t.Fatal("stopped agent resurrected:", string(got))
	}
	if _, err := os.Stat(park); intent == "parked" && errors.Is(err, os.ErrNotExist) {
		t.Fatal("umount consumed park intent")
	}
	if intent == "stopped" && !agent.StopIntentExists() {
		t.Fatal("umount consumed stop intent")
	}
	if intent == "inactive" && !strings.Contains(string(got), "is-active") {
		t.Fatal("inactive service was never checked")
	}
	after, err := config.Load(resolvedConfigPath())
	if err != nil || after.Mount.Enabled {
		t.Fatal("mount was not disabled", err)
	}
}

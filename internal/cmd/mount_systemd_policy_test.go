package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/service"
)

// Frozen old installer output: deliberately independent of the new template.
const legacySystemdMountUnit = `[Unit]
Description=unarr download daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s start
Restart=always
RestartSec=10
SuccessExitStatus=78
RestartPreventExitStatus=78
Environment=HOME=%s

[Install]
WantedBy=default.target
`

type systemdPolicyFixture struct {
	cfg   config.Config
	data  serviceData
	calls string
	body  []byte
}

func newSystemdPolicyFixture(t *testing.T, legacy bool) systemdPolicyFixture {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("private systemctl process fixture is Linux-specific")
	}
	cfg := isolatedMountConfig(t)
	cfg.Auth.APIKey, cfg.Auth.APIURL, cfg.Auth.Mirrors = "", "http://127.0.0.1:1", nil
	cfg.Mount.Directory = t.TempDir()
	saveMountFixture(t, cfg)
	data, err := resolveServiceData()
	if err != nil {
		t.Fatal(err)
	}
	writeKnownSystemdPolicyUnit(t, data, legacy)
	body, err := os.ReadFile(service.SystemdUnitPathIn(data.Home))
	if err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(t.TempDir(), "calls")
	installSystemdPolicySpy(t, data, calls)
	return systemdPolicyFixture{cfg: cfg, data: data, calls: calls, body: body}
}

func writeKnownSystemdPolicyUnit(t *testing.T, data serviceData, legacy bool) {
	t.Helper()
	path := service.SystemdUnitPathIn(data.Home)
	if !legacy {
		if err := writeServiceFile(path, systemdTemplate, data); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf(legacySystemdMountUnit, data.BinPath, data.Home)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func installSystemdPolicySpy(t *testing.T, data serviceData, calls string) {
	t.Helper()
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	t.Setenv("POLICY_TEST_UNIT", service.SystemdUnitPathIn(data.Home))
	t.Setenv("POLICY_TEST_OWN", systemdMountPolicyPath(data.Home))
	t.Setenv("POLICY_TEST_CALLS", calls)
	t.Setenv("POLICY_TEST_RELOADED", filepath.Join(t.TempDir(), "reloaded"))
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$POLICY_TEST_CALLS"
case "$2" in
show)
  if [ "$POLICY_TEST_SHOW_FAIL" = 1 ]; then exit 42; fi
  if [ "$POLICY_TEST_AFTER_SHOW_FAIL" = 1 ] && [ -f "$POLICY_TEST_RELOADED" ]; then exit 43; fi
  if [ ! -f "$POLICY_TEST_UNIT" ]; then
    printf 'LoadState=not-found\nFragmentPath=\nDropInPaths=\nKillMode=\n'; exit 0
  fi
  fragment="$POLICY_TEST_UNIT"; dropins=""; mode=control-group
  if /bin/grep -q '^KillMode=mixed$' "$POLICY_TEST_UNIT"; then mode=mixed; fi
  if [ -f "$POLICY_TEST_RELOADED" ] && [ -f "$POLICY_TEST_OWN" ]; then dropins="$POLICY_TEST_OWN"; mode=mixed; fi
  if [ -n "$POLICY_TEST_FRAGMENT" ]; then fragment="$POLICY_TEST_FRAGMENT"; fi
  if [ -n "$POLICY_TEST_DROPINS" ]; then dropins="$POLICY_TEST_DROPINS"; fi
  if [ -n "$POLICY_TEST_MODE" ]; then mode="$POLICY_TEST_MODE"; fi
  printf 'LoadState=loaded\nFragmentPath=%s\nDropInPaths=%s\nKillMode=%s\n' "$fragment" "$dropins" "$mode";;
daemon-reload)
  if [ "$POLICY_TEST_EDIT_OWN" = 1 ]; then printf 'user edit\n' > "$POLICY_TEST_OWN"; fi
  if [ "$POLICY_TEST_RELOAD_FAIL" = 1 ]; then exit 44; fi
  /bin/touch "$POLICY_TEST_RELOADED";;
is-active)
  if [ "$MOUNT_TEST_SERVICE_STATE" = active ]; then echo active; else echo inactive; exit 3; fi;;
stop|restart)
  if [ "$POLICY_TEST_STOP_FAIL" = 1 ]; then exit 45; fi
  if [ -n "$MOUNT_TEST_LIVE_RESOURCES" ]; then /bin/rm -f "$MOUNT_TEST_LIVE_RESOURCES"; fi;;
disable|enable|start) ;;
*) exit 41;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func policyCalls(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(body)
}

func executeSystemdPolicyEntry(action string) error {
	switch action {
	case "stop":
		return newDaemonStopCmd().Execute()
	case "restart":
		return newDaemonRestartCmd().Execute()
	case "umount":
		return newUmountCmd().Execute()
	default:
		return newDaemonUninstallCmdReal().Execute()
	}
}

func TestSystemdMountPolicyRealLifecycleEntries(t *testing.T) {
	for _, action := range []string{"stop", "restart", "umount", "uninstall"} {
		t.Run(action, func(t *testing.T) {
			f := newSystemdPolicyFixture(t, true)
			t.Setenv("MOUNT_TEST_SERVICE_STATE", "active")
			// Cleanup works without an API key or a paid-access request.
			if err := executeSystemdPolicyEntry(action); err != nil {
				t.Fatal(err)
			}
			calls := policyCalls(t, f.calls)
			operation := action
			if action == "umount" {
				operation = "restart"
			}
			if action == "uninstall" {
				operation = "stop"
			}
			reload, control := strings.Index(calls, "--user daemon-reload"), strings.Index(calls, "--user "+operation+" unarr\n")
			if reload < 0 || control < reload || strings.Count(calls[:control], "--user show ") < 2 {
				t.Fatalf("policy was not verified before lifecycle operation: %q", calls)
			}
			base := service.SystemdUnitPathIn(f.data.Home)
			if action == "uninstall" {
				if _, err := os.Stat(systemdMountPolicyPath(f.data.Home)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("uninstall kept its own recognized policy", err)
				}
			} else {
				body, err := os.ReadFile(base)
				if err != nil || string(body) != string(f.body) {
					t.Fatal("legacy base unit changed", err)
				}
				if !recognizedSystemdMountPolicy(systemdMountPolicyPath(f.data.Home)) {
					t.Fatal("owned mixed override is missing")
				}
			}
		})
	}
}

func TestSystemdMountPolicyFailureBlocksRealEntriesBeforeMutation(t *testing.T) {
	for _, action := range []string{"stop", "restart", "umount", "uninstall"} {
		t.Run(action, func(t *testing.T) {
			f := newSystemdPolicyFixture(t, true)
			t.Setenv("POLICY_TEST_AFTER_SHOW_FAIL", "1")
			t.Setenv("MOUNT_TEST_SERVICE_STATE", "active")
			before := saveMountFixture(t, f.cfg)
			if action != "umount" {
				if err := os.WriteFile(parkedMarkerPath(), []byte("parked"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := executeSystemdPolicyEntry(action); err == nil {
				t.Fatal("unverified policy reported success")
			}
			calls := policyCalls(t, f.calls)
			for _, forbidden := range []string{"--user stop unarr\n", "--user restart unarr\n", "--user disable unarr\n", "--user start unarr\n"} {
				if strings.Contains(calls, forbidden) {
					t.Fatalf("failed policy invoked %q: %q", forbidden, calls)
				}
			}
			after, err := os.ReadFile(resolvedConfigPath())
			if err != nil || string(after) != string(before) {
				t.Fatal("failed policy changed persisted mount intention", err)
			}
			if action != "umount" && !parkedMarkerExists() {
				t.Fatal("failed policy consumed parked intention")
			}
			if _, err := os.Stat(systemdMountPolicyPath(f.data.Home)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed policy was not rolled back", err)
			}
			body, err := os.ReadFile(service.SystemdUnitPathIn(f.data.Home))
			if err != nil || string(body) != string(f.body) {
				t.Fatal("failure changed legacy base", err)
			}
		})
	}
}

func TestSystemdMountPolicyRejectsForeignIdentityAndFiles(t *testing.T) {
	for _, mode := range []string{"custom", "fragment", "base-symlink", "directory-symlink", "external", "edited-own", "effective-external", "absent-base-external", "absent-base-edited-own"} {
		t.Run(mode, func(t *testing.T) {
			f := newSystemdPolicyFixture(t, true)
			base := service.SystemdUnitPathIn(f.data.Home)
			own := systemdMountPolicyPath(f.data.Home)
			preserve := base
			switch mode {
			case "custom":
				if err := os.WriteFile(base, []byte("[Service]\nExecStart=/custom/service\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "fragment":
				t.Setenv("POLICY_TEST_FRAGMENT", "/another/unarr.service")
			case "base-symlink":
				preserve = filepath.Join(t.TempDir(), "unit")
				if err := os.Rename(base, preserve); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(preserve, base); err != nil {
					t.Fatal(err)
				}
			case "directory-symlink":
				if err := os.Symlink(t.TempDir(), filepath.Dir(own)); err != nil {
					t.Fatal(err)
				}
			case "external", "edited-own", "absent-base-external", "absent-base-edited-own":
				if err := os.MkdirAll(filepath.Dir(own), 0o700); err != nil {
					t.Fatal(err)
				}
				preserve = own
				if mode == "external" || mode == "absent-base-external" {
					preserve = filepath.Join(filepath.Dir(own), "10-custom.conf")
				}
				if err := os.WriteFile(preserve, []byte("[Service]\nEnvironment=CUSTOM=1\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if strings.HasPrefix(mode, "absent-base-") {
					if err := os.Remove(base); err != nil {
						t.Fatal(err)
					}
				}
			case "effective-external":
				t.Setenv("POLICY_TEST_DROPINS", "/etc/systemd/user/service.d/custom.conf")
			}
			before, err := os.ReadFile(preserve)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := guardDefaultSystemdMountPolicy(); err == nil {
				t.Fatal("foreign identity/policy accepted")
			}
			if strings.Contains(policyCalls(t, f.calls), "daemon-reload") {
				t.Fatal("rejected policy reloaded the service")
			}
			after, err := os.ReadFile(preserve)
			if err != nil || string(after) != string(before) {
				t.Fatal("foreign bytes changed", err)
			}
		})
	}
}

func TestSystemdMountPolicyRollbackAndWriteFailure(t *testing.T) {
	for _, mode := range []string{"write", "reload", "show", "not-adopted", "concurrent-edit"} {
		t.Run(mode, func(t *testing.T) {
			f := newSystemdPolicyFixture(t, true)
			var err error
			switch mode {
			case "write":
				err = applySystemdMountPolicy(f.data, true, func(string, string, serviceData) error { return fs.ErrPermission })
				if !errors.Is(err, fs.ErrPermission) {
					t.Fatal("write error lost", err)
				}
			case "reload":
				t.Setenv("POLICY_TEST_RELOAD_FAIL", "1")
			case "show":
				t.Setenv("POLICY_TEST_AFTER_SHOW_FAIL", "1")
			case "not-adopted":
				t.Setenv("POLICY_TEST_MODE", "control-group")
			case "concurrent-edit":
				t.Setenv("POLICY_TEST_RELOAD_FAIL", "1")
				t.Setenv("POLICY_TEST_EDIT_OWN", "1")
			}
			if mode != "write" {
				_, err = guardDefaultSystemdMountPolicy()
			}
			if err == nil {
				t.Fatal("policy failure reported success")
			}
			own, readErr := os.ReadFile(systemdMountPolicyPath(f.data.Home))
			if mode == "concurrent-edit" {
				if readErr != nil || string(own) != "user edit\n" {
					t.Fatal("rollback deleted a concurrent edit", readErr)
				}
			} else if !errors.Is(readErr, os.ErrNotExist) {
				t.Fatal("rollback retained new policy", readErr)
			}
			body, readErr := os.ReadFile(service.SystemdUnitPathIn(f.data.Home))
			if readErr != nil || string(body) != string(f.body) {
				t.Fatal("rollback changed base bytes", readErr)
			}
		})
	}
}

func TestSystemdMountPolicyNewLegacyRepeatAndUninstallOwnership(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint(legacy), func(t *testing.T) {
			f := newSystemdPolicyFixture(t, legacy)
			for i := 0; i < 2; i++ {
				if _, err := guardDefaultSystemdMountPolicy(); err != nil {
					t.Fatal(err)
				}
			}
			wantReloads := 0
			if legacy {
				wantReloads = 1
			}
			if n := strings.Count(policyCalls(t, f.calls), "daemon-reload"); n != wantReloads {
				t.Fatalf("reloads=%d want=%d", n, wantReloads)
			}
		})
	}
	t.Run("preserve modified own and foreign overrides", func(t *testing.T) {
		f := newSystemdPolicyFixture(t, false)
		own := systemdMountPolicyPath(f.data.Home)
		if err := os.MkdirAll(filepath.Dir(own), 0o700); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{own, filepath.Join(filepath.Dir(own), "user.conf")} {
			if err := os.WriteFile(name, []byte("user policy"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := removeOwnedSystemdMountPolicy(f.data.Home); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{own, filepath.Join(filepath.Dir(own), "user.conf")} {
			if b, err := os.ReadFile(name); err != nil || string(b) != "user policy" {
				t.Fatal("uninstall removed foreign policy", err)
			}
		}
	})
}

func TestSystemdMountPolicyUsesPersistedServiceConfigAndLeavesNoMountCustomAlone(t *testing.T) {
	f := newSystemdPolicyFixture(t, true)
	t.Setenv("UNARR_CONFIG_DIR", t.TempDir())
	cfgFile = filepath.Join(t.TempDir(), "other.toml")
	appCfg.Mount.Enabled, appCfg.Mount.Directory = false, ""
	if _, err := guardDefaultSystemdMountPolicy(); err != nil {
		t.Fatal(err)
	}
	if !recognizedSystemdMountPolicy(systemdMountPolicyPath(f.data.Home)) {
		t.Fatal("shell override hid persisted service mount")
	}
	t.Run("no mount custom service", func(t *testing.T) {
		f := newSystemdPolicyFixture(t, false)
		f.cfg.Mount.Enabled, f.cfg.Mount.Directory = false, ""
		saveMountFixture(t, f.cfg)
		if err := os.WriteFile(service.SystemdUnitPathIn(f.data.Home), []byte("custom normal service"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, action := range []string{"stop", "restart"} {
			if err := executeSystemdPolicyEntry(action); err != nil {
				t.Fatal(err)
			}
		}
		if strings.Contains(policyCalls(t, f.calls), "show") || strings.Contains(policyCalls(t, f.calls), "daemon-reload") {
			t.Fatal("no-mount custom lifecycle was changed")
		}
	})
}

func TestSystemdMountPolicyPaidDenialPrecedesServiceInspection(t *testing.T) {
	f := newSystemdPolicyFixture(t, true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer srv.Close()
	f.cfg.Auth.APIKey, f.cfg.Auth.APIURL, f.cfg.Auth.Mirrors = "synthetic-key", srv.URL, nil
	before := saveMountFixture(t, f.cfg)
	cmd := newMountCmd()
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{t.TempDir()})
	if err := cmd.Execute(); err == nil {
		t.Fatal("paid denial enabled mount")
	}
	if calls := policyCalls(t, f.calls); calls != "" {
		t.Fatal("paid denial inspected or changed service", calls)
	}
	if after, err := os.ReadFile(resolvedConfigPath()); err != nil || string(after) != string(before) {
		t.Fatal("paid denial saved activation", err)
	}
}

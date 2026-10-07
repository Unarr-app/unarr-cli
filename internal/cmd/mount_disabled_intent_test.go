package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
)

// The installed service previously read Enabled=true. Saving false does not
// reload its mount session; explicit Cobra umount must still request teardown.
// This fixture proves service-manager reconciliation, not a native FUSE mount.
func TestUmountReconcilesSavedIntentWithService(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("isolated systemctl fixture; native platform teardown is tested separately")
	}
	for _, enabled := range []bool{false, true} {
		name := map[bool]string{false: "saved-disabled", true: "saved-enabled"}[enabled]
		for _, state := range []string{"active", "inactive", "parked", "stopped"} {
			t.Run(name+"/"+state, func(t *testing.T) {
				verifyUmountSavedIntent(t, enabled, state)
			})
		}
	}
}

func verifyUmountSavedIntent(t *testing.T, enabled bool, state string) {
	t.Helper()
	cfg := isolatedMountConfig(t)
	cfg.Mount.Directory = t.TempDir()
	saveMountFixture(t, cfg) // The service's previous mounted configuration.
	data, err := resolveServiceData()
	if err != nil {
		t.Fatal(err)
	}
	writeKnownSystemdPolicyUnit(t, data, false)
	calls := filepath.Join(t.TempDir(), "calls")
	live := filepath.Join(t.TempDir(), "synthetic-dav-and-rclone")
	if state == "active" {
		if err := os.WriteFile(live, []byte("previously mounted"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("MOUNT_TEST_SERVICE_CALLS", calls)
	t.Setenv("MOUNT_TEST_SERVICE_STATE", state)
	t.Setenv("MOUNT_TEST_LIVE_RESOURCES", live)
	installSystemdPolicySpy(t, data, calls)
	if state == "parked" {
		if err := os.WriteFile(parkedMarkerPath(), []byte("parked"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if state == "stopped" {
		agent.WriteStopIntent()
	}
	cfg.Mount.Enabled = enabled
	saveMountFixture(t, cfg) // Mirrors the setting saved by config mount.
	command := newUmountCmd()
	command.SetContext(context.Background())
	command.SetArgs([]string{})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(calls)
	restarted := strings.Contains(string(got), "--user restart unarr")
	if restarted != (state == "active") {
		t.Fatalf("saved enabled=%v service=%s: restart=%v calls=%q", enabled, state, restarted, got)
	}
	if strings.Contains(string(got), "--user start ") {
		t.Fatalf("umount started a service: %q", got)
	}
	if state == "active" {
		if _, err := os.Stat(live); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("previous mount resources never reached teardown", err)
		}
	}
	if state == "active" || state == "inactive" {
		if !strings.Contains(string(got), "is-active") {
			t.Fatal("saved intention bypassed runtime service check")
		}
	}
	if state == "parked" && !parkedMarkerExists() {
		t.Fatal("umount consumed parked intention")
	}
	if state == "stopped" && !agent.StopIntentExists() {
		t.Fatal("umount consumed stopped intention")
	}
	after, err := config.Load(resolvedConfigPath())
	if err != nil || after.Mount.Enabled {
		t.Fatal("mount intention was not disabled", err)
	}
}

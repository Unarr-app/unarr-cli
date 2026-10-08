package mountsetup

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Only on an explicitly authorized test machine: installs/reinstalls a system
// driver even if one already exists. Normal test runs never change drivers.
func TestOfficialDriverInstallation(t *testing.T) {
	if os.Getenv("UNARR_TEST_DRIVER_INSTALL") != "1" {
		t.Skip("requires explicit test-machine installation authorization")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	t.Log("EXPLICIT TEST INSTALLATION:", driverExplanation(runtime.GOOS))
	if err := installDriver(ctx, Options{Directory: t.TempDir(), Output: os.Stdout}); err != nil {
		t.Fatal(err)
	}
	if err := driverReady(); err != nil {
		t.Fatal(err)
	}
	t.Log("Driver files verified; activation/reboot is a separate runtime check")
}

func TestNativeDriverFailure(t *testing.T) {
	want := os.Getenv("UNARR_TEST_DRIVER_FAILURE")
	if want == "" {
		t.Skip("opt-in isolated host failure")
	}
	opts := Options{Directory: t.TempDir(), Output: os.Stdout, Confirm: func(s string) error { t.Log("TEST APPROVAL:", s); return nil }}
	err := ensureDriver(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("wanted %q, got %v", want, err)
	}
	t.Log("Expected host constraint reported:", err)
}

func TestInstalledDriverState(t *testing.T) {
	if os.Getenv("UNARR_TEST_DRIVER_STATE") != "1" {
		t.Skip("opt-in native driver probe")
	}
	if err := driverReady(); err != nil {
		t.Fatal(err)
	}
	t.Log("native driver files are present")
}

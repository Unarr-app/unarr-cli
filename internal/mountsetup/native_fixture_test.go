package mountsetup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// This opt-in fixture invokes only the existing official binary/checksum path.
// It deliberately cannot install, activate or approve a filesystem driver.
func TestNativePrepareRclone(t *testing.T) {
	if os.Getenv("UNARR_NATIVE_PREPARE_RCLONE") != "1" {
		t.Skip("explicit native rclone preparation opt-in required")
	}
	dir := os.Getenv("UNARR_NATIVE_TOOLS_DIR")
	if !filepath.IsAbs(dir) {
		t.Fatal("UNARR_NATIVE_TOOLS_DIR must be an absolute isolated fixture directory")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	binary, err := ensureRclone(ctx, Options{Directory: dir, Output: os.Stdout})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("NATIVE_RCLONE=%s", binary)
}

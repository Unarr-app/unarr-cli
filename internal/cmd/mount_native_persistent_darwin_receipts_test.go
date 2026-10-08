//go:build darwin

package cmd

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

// Retain bounded synthetic daemon/rclone diagnostics in the raw test receipt
// before removing the private HOME. Never inspect real-user logs.
func (g *nativeMacPersistent) logReceipt(t *testing.T) {
	t.Helper()
	for _, p := range []string{filepath.Join(config.DataDir(), "unarr.log"), daemonBootLogPath()} {
		nativeMacPrivatePath(t, g.home, p)
		info, err := os.Lstat(p)
		if err != nil || !info.Mode().IsRegular() {
			t.Logf("private daemon log %s unavailable: %v", p, err)
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			t.Errorf("private log receipt: %v", err)
			continue
		}
		b, err := io.ReadAll(io.LimitReader(f, 128<<10))
		_ = f.Close()
		t.Logf("private daemon/rclone log %s size=%d capped=131072 read=%v:\n%s", p, info.Size(), err, b)
	}
}

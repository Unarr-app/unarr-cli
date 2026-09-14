package engine

import (
	"context"
	"os"
	"strconv"
	"testing"
)

// TestHarnessForbiddenListenPort runs ONLY from test/windows/smoke-portrange.ps1,
// which reserves the port named in UNARR_HARNESS_FORBIDDEN_PORT with
// `netsh int ipv4 add excludedportrange protocol=udp` — the reservation Hyper-V,
// WinNAT and Docker Desktop make on real machines. Plain `go test` skips it.
//
// Before nextListenPort learned WSAEACCES, NewTorrentDownloader treated the
// refused UDP bind as fatal, so on such a machine the torrent engine never
// started. It must now come up on a port outside the reserved block.
func TestHarnessForbiddenListenPort(t *testing.T) {
	raw := os.Getenv("UNARR_HARNESS_FORBIDDEN_PORT")
	if raw == "" {
		t.Skip("harness-only: UNARR_HARNESS_FORBIDDEN_PORT names a port Windows has reserved")
	}
	port, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("UNARR_HARNESS_FORBIDDEN_PORT=%q: %v", raw, err)
	}
	dl, err := NewTorrentDownloader(TorrentConfig{DataDir: t.TempDir(), ListenPort: port})
	if err != nil {
		t.Fatalf("the torrent engine did not start with port %d reserved by Windows: %v", port, err)
	}
	if err := dl.Shutdown(context.Background()); err != nil {
		t.Logf("shutdown: %v", err)
	}
}

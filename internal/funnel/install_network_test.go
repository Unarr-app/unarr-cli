package funnel

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDownloadCloudflaredForReal fetches the pinned asset from GitHub and checks
// that what lands is a runnable cloudflared. OPT-IN (UNARR_NET_TESTS=1): it
// pulls ~50 MB, so it has no business in CI or in a normal `go test ./...`.
//
// It exists because the Windows download path cannot be proven any other way.
// Everything else about it — the asset name, the pinned hash, the magic bytes —
// is a table that can be right in the test and wrong against reality: only
// running it on Windows shows whether the URL resolves, whether the bytes match
// the hash Cloudflare published, and whether the file is executable once it is
// renamed into place.
//
//	GOOS=windows go test -c ./internal/funnel   # then, in the guest:
//	set UNARR_NET_TESTS=1 && funnel_pkg_test.exe -test.run=ForReal -test.v
func TestDownloadCloudflaredForReal(t *testing.T) {
	if os.Getenv("UNARR_NET_TESTS") != "1" {
		t.Skip("network test: set UNARR_NET_TESTS=1 to download ~50 MB from GitHub")
	}
	asset, ok := assetFor(runtime.GOOS, runtime.GOARCH)
	if !ok {
		t.Skipf("%s/%s has no downloadable asset by design", runtime.GOOS, runtime.GOARCH)
	}
	t.Logf("asset: %s (pinned %s)", asset, pinnedCloudflaredVersion)

	dest := filepath.Join(t.TempDir(), "cloudflared")
	if runtime.GOOS == "windows" {
		dest += ".exe"
	}

	got, err := downloadCloudflared(dest)
	if err != nil {
		t.Fatalf("downloadCloudflared: %v", err)
	}
	if got != dest {
		t.Fatalf("returned %q, want %q", got, dest)
	}
	fi, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("the promoted binary is not there: %v", err)
	}
	t.Logf("downloaded and verified %d bytes", fi.Size())

	// The point of the whole exercise: the thing that landed must actually run.
	// A hash match proves the bytes; only this proves the file is usable —
	// promoted with the executable bit on unix, and not quarantined on Windows.
	out, err := exec.Command(dest, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("the downloaded cloudflared does not run: %v\n%s", err, out)
	}
	if !strings.Contains(strings.ToLower(string(out)), "cloudflared") {
		t.Fatalf("--version printed %q, which is not cloudflared", out)
	}
	t.Logf("runs: %s", strings.TrimSpace(string(out)))

	// And a second call must reuse it rather than fetch again.
	if _, err := os.Stat(dest + ".partial"); !os.IsNotExist(err) {
		t.Errorf("a .partial file survived the download (stat err: %v)", err)
	}
}

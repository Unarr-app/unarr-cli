package funnel

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// TestAssetForCoversTheShippedPlatforms pins which hosts can fetch cloudflared
// for themselves. Windows amd64 is here because Cloudflare ships it as a bare
// .exe; before that, a Windows box with the funnel enabled retried a download
// that the code KNEW could never happen, every five minutes, forever.
func TestAssetForCoversTheShippedPlatforms(t *testing.T) {
	cases := []struct {
		goos, goarch string
		want         string
	}{
		{"linux", "amd64", "cloudflared-linux-amd64"},
		{"linux", "arm64", "cloudflared-linux-arm64"},
		{"linux", "arm", "cloudflared-linux-armhf"},
		{"linux", "386", "cloudflared-linux-386"},
		{"windows", "amd64", "cloudflared-windows-amd64.exe"},
		// No upstream asset / no shipped build: these must NOT invent a name.
		{"windows", "arm64", ""},
		{"windows", "386", ""},
		{"darwin", "arm64", ""}, // .tgz, deliberately out of scope
		{"darwin", "amd64", ""},
		{"freebsd", "amd64", ""},
	}
	for _, tc := range cases {
		t.Run(tc.goos+"/"+tc.goarch, func(t *testing.T) {
			got, ok := assetFor(tc.goos, tc.goarch)
			if tc.want == "" {
				if ok {
					t.Fatalf("assetFor(%s, %s) = %q, want no asset", tc.goos, tc.goarch, got)
				}
				return
			}
			if !ok || got != tc.want {
				t.Fatalf("assetFor(%s, %s) = %q, %v; want %q, true", tc.goos, tc.goarch, got, ok, tc.want)
			}
		})
	}
}

// TestEveryDownloadableAssetIsPinned is the invariant that keeps the integrity
// gate honest: naming an asset without pinning its SHA-256 would mean a platform
// that downloads 50 MB and then refuses it at the last step — or, if the check
// were ever loosened, runs unverified bytes.
func TestEveryDownloadableAssetIsPinned(t *testing.T) {
	platforms := []struct{ goos, goarch string }{
		{"linux", "amd64"}, {"linux", "arm64"}, {"linux", "arm"}, {"linux", "386"},
		{"windows", "amd64"}, {"windows", "arm64"}, {"windows", "386"},
		{"darwin", "amd64"}, {"darwin", "arm64"},
	}
	named := map[string]bool{}
	for _, p := range platforms {
		asset, ok := assetFor(p.goos, p.goarch)
		if !ok {
			continue
		}
		named[asset] = true
		sha, pinned := pinnedCloudflaredSHA256[asset]
		if !pinned {
			t.Errorf("%s/%s downloads %q, which has no pinned SHA-256", p.goos, p.goarch, asset)
			continue
		}
		if len(sha) != 64 {
			t.Errorf("%s: SHA-256 is %d chars, want 64", asset, len(sha))
		}
		if strings.ToLower(sha) != sha {
			t.Errorf("%s: SHA-256 must be lower-case hex for the case-insensitive compare to read plainly", asset)
		}
	}
	// And the other direction: a hash for an asset nothing can ask for is dead
	// weight that will silently rot across a version bump.
	for asset := range pinnedCloudflaredSHA256 {
		if !named[asset] {
			t.Errorf("pinned SHA-256 for %q, but assetFor never names it", asset)
		}
	}
}

// TestUnsupportedPlatformIsASentinelWithAnActionableHint: the supervisor keys
// its stop-retrying decision off this error, and the user keys their next move
// off its text.
func TestUnsupportedPlatformIsASentinelWithAnActionableHint(t *testing.T) {
	if _, ok := assetFor(runtime.GOOS, runtime.GOARCH); ok {
		t.Skipf("%s/%s can download cloudflared; this covers the platforms that cannot",
			runtime.GOOS, runtime.GOARCH)
	}
	_, err := downloadCloudflared(t.TempDir() + "/cloudflared")
	if err == nil {
		t.Fatal("a platform with no asset must not report success")
	}
	if !errors.Is(err, ErrNoAutoDownload) {
		t.Fatalf("err = %v, which the supervisor cannot recognise; it must wrap ErrNoAutoDownload", err)
	}
	if !strings.Contains(err.Error(), installHint()) {
		t.Fatalf("err = %v, missing the install command for this OS (%q)", err, installHint())
	}
}

// TestInstallHintIsASCII: this text is logged, and unarr.log is read back by a
// CP850 console, a BOM-less Notepad and the crash-report pipeline. See
// internal/logging.TestLogLinesAreASCII.
func TestInstallHintIsASCII(t *testing.T) {
	for _, goos := range []string{"windows", "darwin", "linux"} {
		hint := hintFor(goos)
		for i := 0; i < len(hint); i++ {
			if hint[i] > 0x7F {
				t.Fatalf("%s hint has a non-ASCII byte at %d: %q", goos, i, hint)
			}
		}
		if hint == "" {
			t.Fatalf("%s has no install hint, so the error tells the user nothing", goos)
		}
	}
}

// hintFor is installHint for an explicit GOOS, so every branch is reachable from
// one test run instead of only the host's.
func hintFor(goos string) string {
	switch goos {
	case "windows":
		return "winget install --id Cloudflare.cloudflared"
	case "darwin":
		return "brew install cloudflared"
	default:
		return "see https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/"
	}
}

// TestHintForMatchesInstallHint keeps the test's copy honest about the real one.
func TestHintForMatchesInstallHint(t *testing.T) {
	if got, want := hintFor(runtime.GOOS), installHint(); got != want {
		t.Fatalf("hintFor(%s) = %q, installHint() = %q", runtime.GOOS, got, want)
	}
}

// TestExecutableMagicMatchesTheAssetShape: the cheap pre-hash guard must know
// the format of every asset it can be handed, or it rejects a good download.
func TestExecutableMagicMatchesTheAssetShape(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		if _, ok := assetFor(goos, "amd64"); !ok {
			continue
		}
		magic, ok := executableMagic(goos)
		if !ok || len(magic) == 0 {
			t.Fatalf("%s downloads an executable but has no magic bytes to sanity-check it", goos)
		}
	}
	if _, ok := executableMagic("plan9"); ok {
		t.Fatal("a platform with no known signature must report none, not a wrong one")
	}
}

// TestPinnedVersionLooksLikeARelease guards the bump procedure: the URL is built
// from this constant, so a stray "v" or an empty value is a 404 for every host.
func TestPinnedVersionLooksLikeARelease(t *testing.T) {
	if pinnedCloudflaredVersion == "" || strings.HasPrefix(pinnedCloudflaredVersion, "v") {
		t.Fatalf("pinnedCloudflaredVersion = %q; upstream tags are bare dates like 2026.5.2",
			pinnedCloudflaredVersion)
	}
	if n := strings.Count(pinnedCloudflaredVersion, "."); n != 2 {
		t.Fatalf("pinnedCloudflaredVersion = %q, want YYYY.M.P", pinnedCloudflaredVersion)
	}
	// The URL this builds is the one thing a bump can silently break.
	asset, _ := assetFor("linux", "amd64")
	url := fmt.Sprintf("https://github.com/cloudflare/cloudflared/releases/download/%s/%s",
		pinnedCloudflaredVersion, asset)
	if strings.Contains(url, "//releases") || strings.HasSuffix(url, "/") {
		t.Fatalf("malformed download URL: %s", url)
	}
}

package funnel

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// TestVerifySHA256 covers the integrity gate used on the auto-downloaded
// cloudflared binary: it accepts the matching digest (case-insensitive) and
// rejects a wrong one.
func TestVerifySHA256(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blob")
	content := []byte("cloudflared-bytes")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	good := hex.EncodeToString(sum[:])

	if err := verifySHA256(path, good); err != nil {
		t.Errorf("verifySHA256(correct) = %v, want nil", err)
	}
	// Upper-case should still match.
	if err := verifySHA256(path, good[:60]+"ABCD"); err == nil {
		t.Error("verifySHA256(wrong) = nil, want mismatch error")
	}
	if err := verifySHA256(path, "deadbeef"); err == nil {
		t.Error("verifySHA256(short/wrong) = nil, want error")
	}
}

// TestPinnedCloudflaredSHA256Complete guards the invariant that every linux arch
// the downloader can select has a pinned 64-hex SHA-256, so a download never
// reaches the verify step without an expected digest.
func TestPinnedCloudflaredSHA256Complete(t *testing.T) {
	wantAssets := []string{
		"cloudflared-linux-amd64",
		"cloudflared-linux-arm64",
		"cloudflared-linux-armhf",
		"cloudflared-linux-386",
	}
	for _, a := range wantAssets {
		sum, ok := pinnedCloudflaredSHA256[a]
		if !ok {
			t.Errorf("missing pinned SHA-256 for %q", a)
			continue
		}
		if len(sum) != 64 {
			t.Errorf("%s: SHA-256 length = %d, want 64", a, len(sum))
		}
		if _, err := hex.DecodeString(sum); err != nil {
			t.Errorf("%s: SHA-256 not valid hex: %v", a, err)
		}
	}
	if pinnedCloudflaredVersion == "" {
		t.Error("pinnedCloudflaredVersion must be set")
	}
}

// TestRemoveStaleCached: a bump must leave only the current binary, without
// touching another daemon's in-flight .partial download.
func TestRemoveStaleCached(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "cloudflared-2026.10.5")
	for _, n := range []string{"cloudflared-2026.10.5", "cloudflared-2026.5.2", "cloudflared", "cloudflared-2026.10.5.partial", "other"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	removeStaleCached(current)
	for n, want := range map[string]bool{
		"cloudflared-2026.10.5": true, "cloudflared-2026.10.5.partial": true, "other": true,
		"cloudflared-2026.5.2": false, "cloudflared": false,
	} {
		if _, err := os.Stat(filepath.Join(dir, n)); (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", n, err == nil, want)
		}
	}
}

package upgrade

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDesktopAssetName(t *testing.T) {
	tests := []struct {
		version, goos, goarch, want string
	}{
		{"1.6.0", "linux", "amd64", "unarr-desktop_1.6.0_linux_amd64"},
		{"1.6.0", "darwin", "arm64", "unarr-desktop_1.6.0_darwin_arm64"},
		{"1.6.0", "darwin", "amd64", "unarr-desktop_1.6.0_darwin_amd64"},
		{"1.6.0", "windows", "amd64", "unarr-desktop_1.6.0_windows_amd64.exe"},
		{"2.0.1-beta", "linux", "amd64", "unarr-desktop_2.0.1-beta_linux_amd64"},
	}
	for _, tt := range tests {
		if got := desktopAssetName(tt.version, tt.goos, tt.goarch); got != tt.want {
			t.Errorf("desktopAssetName(%s,%s,%s) = %q, want %q", tt.version, tt.goos, tt.goarch, got, tt.want)
		}
	}
}

func TestIsNewer(t *testing.T) {
	tests := []struct {
		current, candidate string
		want               bool
	}{
		{"1.5.2", "1.6.0", true},
		{"1.6.0", "1.5.2", false},
		{"1.6.0", "1.6.0", false},
		{"1.6.0", "v1.6.1", true},
		{"v1.6.0-beta", "1.6.1", true}, // prerelease suffix ignored, like the web gate
		{"dev", "1.0.0", true},         // non-numeric parses as 0.0.0 — callers gate "dev" themselves
	}
	for _, tt := range tests {
		if got := IsNewer(tt.current, tt.candidate); got != tt.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", tt.current, tt.candidate, got, tt.want)
		}
	}
}

func TestFindDesktopSibling(t *testing.T) {
	dir := t.TempDir()
	if p, ok := FindDesktopSibling(dir); ok {
		t.Fatalf("FindDesktopSibling(empty dir) = (%q, true), want not found", p)
	}
	name := desktopBinaryFilename(runtime.GOOS)
	want := filepath.Join(dir, name)
	if err := os.WriteFile(want, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	p, ok := FindDesktopSibling(dir)
	if !ok || p != want {
		t.Fatalf("FindDesktopSibling() = (%q, %v), want (%q, true)", p, ok, want)
	}
	// A directory with the binary's name must not count as a sibling.
	dir2 := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir2, name), 0o755); err != nil {
		t.Fatal(err)
	}
	if p, ok := FindDesktopSibling(dir2); ok {
		t.Fatalf("FindDesktopSibling(dir-as-binary) = (%q, true), want not found", p)
	}
}

// desktopRelease is an httptest fixture: a release serving the host-platform
// desktop asset plus a checksums-desktop manifest signed with a fresh test
// keypair — the same trust chain production uses, end to end.
type desktopRelease struct {
	version  string
	payload  []byte
	manifest []byte
	sig      []byte
	// omit lets each test 404 selected assets (missing manifest / sig / binary).
	omit map[string]bool
	// hits counts every asset request — the marker-skip test pins "already
	// current means ZERO network traffic".
	hits int
}

func newDesktopRelease(t *testing.T, version string, payload []byte) *desktopRelease {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate keypair: %v", err)
	}
	withReleasePubKey(t, base64.StdEncoding.EncodeToString(pub))

	assetName := desktopAssetName(version, runtime.GOOS, runtime.GOARCH)
	sum := sha256.Sum256(payload)
	manifest := []byte(hex.EncodeToString(sum[:]) + "  " + assetName + "\n")
	return &desktopRelease{
		version:  version,
		payload:  payload,
		manifest: manifest,
		sig:      []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, manifest)) + "\n"),
		omit:     map[string]bool{},
	}
}

func (r *desktopRelease) serve(t *testing.T) {
	t.Helper()
	assetName := desktopAssetName(r.version, runtime.GOOS, runtime.GOARCH)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.hits++
		file := filepath.Base(req.URL.Path)
		if r.omit[file] {
			http.NotFound(w, req)
			return
		}
		switch file {
		case assetName:
			w.Write(r.payload)
		case "checksums-desktop.txt":
			w.Write(r.manifest)
		case "checksums-desktop.txt.sig":
			w.Write(r.sig)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	withReleaseHost(t, srv.URL)
}

// withDesktopSmokeStub bypasses executing the (non-binary) fixture payload.
func withDesktopSmokeStub(t *testing.T) {
	t.Helper()
	prev := desktopSmokeTest
	desktopSmokeTest = func(path, version string) error { return nil }
	t.Cleanup(func() { desktopSmokeTest = prev })
}

// installTarget writes a fake "currently installed" desktop binary and
// returns its path.
func installTarget(t *testing.T) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), desktopBinaryFilename(runtime.GOOS))
	if err := os.WriteFile(target, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return target
}

func TestUpdateDesktopBinaryHappyPath(t *testing.T) {
	rel := newDesktopRelease(t, "9.9.9", []byte("new-desktop-binary-payload"))
	rel.serve(t)
	withDesktopSmokeStub(t)
	target := installTarget(t)

	applied, err := UpdateDesktopBinary(context.Background(), target, "v9.9.9", nil)
	if err != nil || !applied {
		t.Fatalf("UpdateDesktopBinary() = (%v, %v), want (true, nil)", applied, err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != string(rel.payload) {
		t.Fatalf("installed binary = %q (%v), want payload", got, err)
	}
	old, err := os.ReadFile(target + ".old")
	if err != nil || string(old) != "old-binary" {
		t.Fatalf("backup = %q (%v), want old-binary at .old", old, err)
	}
	// A successful swap must record what it installed (the marker is what
	// makes the next same-version update a no-op).
	if v, ok := InstalledDesktopVersion(filepath.Dir(target)); !ok || v != "9.9.9" {
		t.Fatalf("marker after install = (%q, %v), want (9.9.9, true)", v, ok)
	}
}

func TestDesktopVersionMarkerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := writeDesktopVersionMarker(dir, "v1.6.0"); err != nil {
		t.Fatal(err)
	}
	// Writer stores the bare version (no "v", no trailing newline).
	raw, err := os.ReadFile(filepath.Join(dir, "unarr-desktop.version"))
	if err != nil || string(raw) != "1.6.0" {
		t.Fatalf("marker file = %q (%v), want exactly %q", raw, err, "1.6.0")
	}
	if v, ok := InstalledDesktopVersion(dir); !ok || v != "1.6.0" {
		t.Fatalf("InstalledDesktopVersion() = (%q, %v), want (1.6.0, true)", v, ok)
	}
}

func TestInstalledDesktopVersionAbsentAndCorrupt(t *testing.T) {
	if v, ok := InstalledDesktopVersion(t.TempDir()); ok {
		t.Fatalf("absent marker = (%q, true), want not found", v)
	}
	write := func(t *testing.T, content string) string {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, desktopVersionMarker), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	// Trailing whitespace is part of the cross-repo contract (a web installer
	// writing with a final newline must still parse).
	if v, ok := InstalledDesktopVersion(write(t, "v1.6.0\n")); !ok || v != "1.6.0" {
		t.Fatalf("trailing-newline marker = (%q, %v), want (1.6.0, true)", v, ok)
	}
	for name, content := range map[string]string{
		"empty":      "",
		"whitespace": "  \n",
		"multiline":  "1.6.0\ngarbage second line",
		"sentence":   "installed by hand last tuesday",
		"huge":       strings.Repeat("9", 100),
	} {
		if v, ok := InstalledDesktopVersion(write(t, content)); ok {
			t.Fatalf("%s marker = (%q, true), want corrupt → not found", name, v)
		}
	}
}

func TestUpdateDesktopBinaryMarkerSkip(t *testing.T) {
	rel := newDesktopRelease(t, "9.9.9", []byte("payload"))
	rel.serve(t)
	withDesktopSmokeStub(t)
	target := installTarget(t)
	if err := writeDesktopVersionMarker(filepath.Dir(target), "9.9.9"); err != nil {
		t.Fatal(err)
	}

	var logs []string
	applied, err := UpdateDesktopBinary(context.Background(), target, "v9.9.9", func(m string) { logs = append(logs, m) })
	if err != nil || applied {
		t.Fatalf("UpdateDesktopBinary(marker current) = (%v, %v), want (false, nil)", applied, err)
	}
	if rel.hits != 0 {
		t.Fatalf("release server hits = %d, want 0 (skip must not download)", rel.hits)
	}
	// Not assertUntouched: here the marker legitimately names the target
	// version — only the binary and the (absent) backup are pinned.
	if got, rerr := os.ReadFile(target); rerr != nil || string(got) != "old-binary" {
		t.Fatalf("installed binary changed on a skip: %q (%v)", got, rerr)
	}
	if _, serr := os.Stat(target + ".old"); serr == nil {
		t.Fatal("backup .old exists after a skip — something swapped binaries")
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "already current") {
		t.Fatalf("logs = %q, want a single 'already current' line", logs)
	}
}

func TestUpdateDesktopBinaryStaleMarkerProceeds(t *testing.T) {
	rel := newDesktopRelease(t, "9.9.9", []byte("payload"))
	rel.serve(t)
	withDesktopSmokeStub(t)
	target := installTarget(t)
	// Marker present but for an OLDER version → full update, marker refreshed.
	if err := writeDesktopVersionMarker(filepath.Dir(target), "1.0.0"); err != nil {
		t.Fatal(err)
	}

	applied, err := UpdateDesktopBinary(context.Background(), target, "9.9.9", nil)
	if err != nil || !applied {
		t.Fatalf("UpdateDesktopBinary(stale marker) = (%v, %v), want (true, nil)", applied, err)
	}
	if v, ok := InstalledDesktopVersion(filepath.Dir(target)); !ok || v != "9.9.9" {
		t.Fatalf("marker after update = (%q, %v), want (9.9.9, true)", v, ok)
	}
}

func TestUpdateDesktopBinaryMissingManifest(t *testing.T) {
	rel := newDesktopRelease(t, "9.9.9", []byte("payload"))
	rel.omit["checksums-desktop.txt"] = true
	rel.serve(t)
	withDesktopSmokeStub(t)
	target := installTarget(t)

	_, err := UpdateDesktopBinary(context.Background(), target, "9.9.9", nil)
	if !errors.Is(err, ErrNoDesktopAssets) {
		t.Fatalf("err = %v, want ErrNoDesktopAssets", err)
	}
	assertUntouched(t, target)
}

func TestUpdateDesktopBinaryMissingSignature(t *testing.T) {
	rel := newDesktopRelease(t, "9.9.9", []byte("payload"))
	rel.omit["checksums-desktop.txt.sig"] = true
	rel.serve(t)
	withDesktopSmokeStub(t)
	target := installTarget(t)

	_, err := UpdateDesktopBinary(context.Background(), target, "9.9.9", nil)
	if err == nil || !strings.Contains(err.Error(), "unsigned") {
		t.Fatalf("err = %v, want explicit unsigned-manifest failure", err)
	}
	assertUntouched(t, target)
}

func TestUpdateDesktopBinaryBadSignature(t *testing.T) {
	rel := newDesktopRelease(t, "9.9.9", []byte("payload"))
	// Signature from a DIFFERENT key: overwrite with garbage signed by no one.
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	rel.sig = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(otherPriv, rel.manifest)) + "\n")
	rel.serve(t)
	withDesktopSmokeStub(t)
	target := installTarget(t)

	_, err := UpdateDesktopBinary(context.Background(), target, "9.9.9", nil)
	if err == nil || !strings.Contains(err.Error(), "verification failed") {
		t.Fatalf("err = %v, want signature verification failure", err)
	}
	assertUntouched(t, target)
}

func TestUpdateDesktopBinarySHAMismatch(t *testing.T) {
	rel := newDesktopRelease(t, "9.9.9", []byte("payload"))
	// Serve a DIFFERENT binary than the (correctly signed) manifest describes —
	// the mirror-swapped-the-binary attack the sha check exists for.
	rel.payload = []byte("tampered-binary")
	rel.serve(t)
	withDesktopSmokeStub(t)
	target := installTarget(t)

	_, err := UpdateDesktopBinary(context.Background(), target, "9.9.9", nil)
	if err == nil || !strings.Contains(err.Error(), "SHA256 mismatch") {
		t.Fatalf("err = %v, want SHA256 mismatch", err)
	}
	assertUntouched(t, target)
}

func TestUpdateDesktopBinaryAssetMissing(t *testing.T) {
	rel := newDesktopRelease(t, "9.9.9", []byte("payload"))
	rel.omit[desktopAssetName("9.9.9", runtime.GOOS, runtime.GOARCH)] = true
	rel.serve(t)
	withDesktopSmokeStub(t)
	target := installTarget(t)

	_, err := UpdateDesktopBinary(context.Background(), target, "9.9.9", nil)
	if !errors.Is(err, ErrNoDesktopAssets) {
		t.Fatalf("err = %v, want ErrNoDesktopAssets", err)
	}
	assertUntouched(t, target)
}

// assertUntouched pins the security invariant shared by every failure test:
// a failed verification must leave the installed binary byte-identical, and
// must not (over)write the version marker — a marker claiming a version that
// never got installed would make every future update to it a false no-op.
func assertUntouched(t *testing.T, target string) {
	t.Helper()
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "old-binary" {
		t.Fatalf("installed binary was touched on a failed update: %q (%v)", got, err)
	}
	if _, err := os.Stat(target + ".old"); err == nil {
		t.Fatal("backup .old exists after a failed update — swap ran before verification?")
	}
	if v, ok := InstalledDesktopVersion(filepath.Dir(target)); ok && v == "9.9.9" {
		t.Fatal("version marker claims the target version after a failed update")
	}
}

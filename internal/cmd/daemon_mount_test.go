package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMountNZBFileNameIsStableAndSafe(t *testing.T) {
	got := mountNZBFileName(`Show/Season: One`, "nzb-123")
	if strings.ContainsAny(got, `/\:*?"<>|`) || !strings.HasSuffix(got, ".nzb") {
		t.Fatalf("unsafe NZB name %q", got)
	}
	if again := mountNZBFileName(`Show/Season: One`, "nzb-123"); again != got {
		t.Fatalf("unstable NZB name: %q != %q", again, got)
	}
}

func TestWriteMountManifestReplacesCompleteFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "release.nzb")
	if err := writeMountManifest(path, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := writeMountManifest(path, []byte("second")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second" {
		t.Fatalf("manifest = %q", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("manifest permissions = %v", info.Mode().Perm())
	}
}

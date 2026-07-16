package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMaybeDeobfuscate_RenamesObfuscated(t *testing.T) {
	dir := t.TempDir()
	obf := filepath.Join(dir, "a1b2c3d4e5f60718.mkv")
	if err := os.WriteFile(obf, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := maybeDeobfuscate(obf, "Movie.2024.1080p.BluRay.x264-GRP", "Movie Title")
	want := filepath.Join(dir, "Movie.2024.1080p.BluRay.x264-GRP.mkv")
	if got != want {
		t.Fatalf("maybeDeobfuscate() = %q, want %q", got, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("renamed file missing: %v", err)
	}
	if _, err := os.Stat(obf); !os.IsNotExist(err) {
		t.Error("original obfuscated file should be gone")
	}
}

func TestMaybeDeobfuscate_KeepsMeaningfulName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Movie.2024.1080p.mkv")
	if err := os.WriteFile(path, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := maybeDeobfuscate(path, "Other.Title", "Other"); got != path {
		t.Fatalf("maybeDeobfuscate() = %q, want unchanged %q", got, path)
	}
}

func TestMaybeDeobfuscate_TitleAlreadyHasExtension(t *testing.T) {
	dir := t.TempDir()
	obf := filepath.Join(dir, "deadbeef00112233.mkv")
	if err := os.WriteFile(obf, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := maybeDeobfuscate(obf, "Movie.2024.1080p.mkv", "")
	want := filepath.Join(dir, "Movie.2024.1080p.mkv")
	if got != want {
		t.Fatalf("maybeDeobfuscate() = %q, want %q (no double extension)", got, want)
	}
}

func TestMaybeDeobfuscate_NoClobber(t *testing.T) {
	dir := t.TempDir()
	obf := filepath.Join(dir, "deadbeef00112233.mkv")
	existing := filepath.Join(dir, "Movie.mkv")
	for _, p := range []string{obf, existing} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := maybeDeobfuscate(obf, "Movie", ""); got != obf {
		t.Fatalf("maybeDeobfuscate() = %q, want original (target exists)", got)
	}
}

func TestMaybeDeobfuscate_EmptyTitleKeepsOriginal(t *testing.T) {
	dir := t.TempDir()
	obf := filepath.Join(dir, "deadbeef00112233.mkv")
	if err := os.WriteFile(obf, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := maybeDeobfuscate(obf, "", ""); got != obf {
		t.Fatalf("maybeDeobfuscate() = %q, want original (no title available)", got)
	}
}

func TestMaybeDeobfuscate_DirectoryUntouched(t *testing.T) {
	dir := t.TempDir()
	if got := maybeDeobfuscate(dir, "Movie", ""); got != dir {
		t.Fatalf("maybeDeobfuscate() = %q, want %q (directories are never renamed)", got, dir)
	}
}

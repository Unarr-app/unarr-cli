//go:build !windows

package mountsetup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMacDriverPartialInstall(t *testing.T) {
	root := t.TempDir()
	if macDriverPresent(root) {
		t.Fatal("empty bundle accepted")
	}
	dir := filepath.Join(root, "Contents", "Resources")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"load_macfuse", "mount_macfuse"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if macDriverPresent(root) {
		t.Fatal("non-executable helpers accepted")
	}
	for _, name := range []string{"load_macfuse", "mount_macfuse"} {
		if err := os.Chmod(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if !macDriverPresent(root) {
		t.Fatal("complete helpers not recognized")
	}
}

func TestArchConsentDisclosesFullUpgrade(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	if err := os.WriteFile(filepath.Join(dir, "pacman"), []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	if explanation := linuxDriverExplanation(); !strings.Contains(explanation, "ALL installed system packages") || !strings.Contains(explanation, "Cancel") {
		t.Fatal("full upgrade was not disclosed", explanation)
	}
}

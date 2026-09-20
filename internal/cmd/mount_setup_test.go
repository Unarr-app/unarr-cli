package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

func TestMountSetupFreeAccountDoesNotInstall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer server.Close()
	cfg := config.Default()
	cfg.Auth.APIURL, cfg.Auth.APIKey = server.URL, "fake-key"
	previous := cfgFile
	t.Cleanup(func() { cfgFile = previous })
	dir := t.TempDir()
	cfgFile = filepath.Join(dir, "config.toml")
	if _, err := prepareMountDependencies(context.Background(), &cfg); err == nil {
		t.Fatal("free account permitted")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatal("dependency setup touched disk before checking access")
	}
}

func TestMountDestinationCreatesOnlyEmptyDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows expects an absent mountpoint")
	}
	dir := filepath.Join(t.TempDir(), "new-folder")
	if _, err := createMountDirectory(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keep"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := createMountDirectory(dir); err == nil {
		t.Fatal("nonempty mountpoint accepted")
	}
	data, err := os.ReadFile(filepath.Join(dir, "keep"))
	if err != nil || string(data) != "content" {
		t.Fatal("existing content changed", err)
	}
}

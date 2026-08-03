package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestValidatePaths_Dangerous(t *testing.T) {
	dangerous := []string{"/", "/etc", "/bin", "/sbin", "/usr", "/lib", "/lib64",
		"/boot", "/dev", "/proc", "/sys", "/var", "/tmp", "/root",
		"/System", "/Library", "/private"}
	if runtime.GOOS == "windows" {
		// The Unix names are not system dirs there — filepath.Abs("/etc") is just
		// C:\etc on the current drive. Assert the entries dangerousPaths really
		// registers for Windows instead of skipping the platform outright.
		dangerous = []string{`C:\`, `C:\Windows`, `C:\Windows\System32`,
			`C:\Program Files`, `C:\Program Files (x86)`}
	}

	for _, d := range dangerous {
		// Test all three path fields
		for _, field := range []string{"download", "movies", "tvshows"} {
			cfg := Default()
			switch field {
			case "download":
				cfg.Download.Dir = d
			case "movies":
				cfg.Organize.MoviesDir = d
			case "tvshows":
				cfg.Organize.TVShowsDir = d
			}
			if err := cfg.ValidatePaths(); err == nil {
				t.Errorf("ValidatePaths() should reject %s=%q", field, d)
			}
		}
	}
}

func TestValidatePaths_HomeRoot(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}

	cfg := Default()
	cfg.Download.Dir = home
	if err := cfg.ValidatePaths(); err == nil {
		t.Errorf("ValidatePaths() should reject home root %q", home)
	}
}

func TestValidatePaths_HiddenDir(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}

	cfg := Default()
	cfg.Download.Dir = filepath.Join(home, ".ssh")
	if err := cfg.ValidatePaths(); err == nil {
		t.Error("ValidatePaths() should reject ~/.ssh")
	}
}

func TestValidatePaths_Valid(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}

	valid := []string{
		filepath.Join(home, "Downloads"),
		filepath.Join(home, "Media"),
		filepath.Join(home, "Media", "Movies"),
		"/mnt/storage/downloads",
	}

	for _, d := range valid {
		cfg := Default()
		cfg.Download.Dir = d
		if err := cfg.ValidatePaths(); err != nil {
			t.Errorf("ValidatePaths() should accept %q, got: %v", d, err)
		}
	}
}

func TestValidatePaths_AllowedHiddenDirs(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}

	// .local and .config are whitelisted
	allowed := []string{
		filepath.Join(home, ".local", "share", "unarr"),
		filepath.Join(home, ".config", "unarr"),
	}

	for _, d := range allowed {
		cfg := Default()
		cfg.Download.Dir = d
		if err := cfg.ValidatePaths(); err != nil {
			t.Errorf("ValidatePaths() should allow %q, got: %v", d, err)
		}
	}
}

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config holds all persistent CLI configuration.
type Config struct {
	Auth          AuthConfig          `toml:"auth"`
	Agent         AgentConfig         `toml:"agent"`
	Download      DownloadConfig      `toml:"downloads"`
	Organize      OrganizeConfig      `toml:"organize"`
	Daemon        DaemonConfig        `toml:"daemon"`
	Notifications NotificationsConfig `toml:"notifications"`
	General       GeneralConfig       `toml:"general"`
	Library       LibraryConfig       `toml:"library"`
}

type AuthConfig struct {
	APIKey string `toml:"api_key"`
	APIURL string `toml:"api_url"`
	WSURL  string `toml:"ws_url"` // optional, derived from api_url if empty
}

type AgentConfig struct {
	ID   string `toml:"id"`
	Name string `toml:"name"`
}

type DownloadConfig struct {
	Dir              string `toml:"dir"`
	PreferredMethod  string `toml:"preferred_method"`
	PreferredQuality string `toml:"preferred_quality"` // "2160p", "1080p", "720p" — hint for auto-selection
	MaxConcurrent    int    `toml:"max_concurrent"`
	MaxDownloadSpeed string `toml:"max_download_speed"` // e.g. "10MB", "500KB", "0" = unlimited
	MaxUploadSpeed   string `toml:"max_upload_speed"`   // e.g. "1MB", "0" = unlimited
	MetadataTimeout  string `toml:"metadata_timeout"`   // e.g. "1h", "30m", "0" = unlimited (default: "0")
	StallTimeout     string `toml:"stall_timeout"`      // e.g. "30m", "1h", "0" = unlimited (default: "30m")
	ListenPort       int    `toml:"listen_port"`        // fixed port for incoming peer connections (default: 42069, 0 = random)
}

type OrganizeConfig struct {
	Enabled    bool   `toml:"enabled"`
	MoviesDir  string `toml:"movies_dir"`
	TVShowsDir string `toml:"tv_shows_dir"`
}

type DaemonConfig struct {
	PollInterval      string `toml:"poll_interval"`
	HeartbeatInterval string `toml:"heartbeat_interval"`
}

type NotificationsConfig struct {
	Enabled bool `toml:"enabled"`
}

type GeneralConfig struct {
	Country string `toml:"country"`
	Locale  string `toml:"locale"`
	NoColor bool   `toml:"no_color"`
}

type LibraryConfig struct {
	ScanPath     string `toml:"scan_path"`     // remembered from last scan
	Workers      int    `toml:"workers"`       // concurrent ffprobe (default 8)
	FFprobePath  string `toml:"ffprobe_path"`  // optional explicit path
	BackupDir    string `toml:"backup_dir"`    // for replaced files
	AutoScan     bool   `toml:"auto_scan"`     // enable daily auto-scan in daemon (default true)
	ScanInterval string `toml:"scan_interval"` // e.g. "24h", "12h", "6h" (default "24h")
}

// Default returns a Config with sensible defaults.
func Default() Config {
	return Config{
		Auth: AuthConfig{
			APIURL: "https://torrentclaw.com",
		},
		Download: DownloadConfig{
			PreferredMethod: "auto",
			MaxConcurrent:   3,
		},
		Organize: OrganizeConfig{
			Enabled: true,
		},
		Daemon: DaemonConfig{
			PollInterval:      "30s",
			HeartbeatInterval: "30s",
		},
		Notifications: NotificationsConfig{
			Enabled: true,
		},
		General: GeneralConfig{
			Country: "US",
			Locale:  "en",
		},
		Library: LibraryConfig{
			AutoScan:     true,
			ScanInterval: "24h",
			Workers:      8,
		},
	}
}

// Load reads config from the default or specified path.
// Falls back to defaults for any missing values.
// If the file does not exist, returns defaults without error.
func Load(path string) (Config, error) {
	if path == "" {
		path = FilePath()
	}

	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read config: %w", err)
	}

	if err := toml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}

	// Re-apply defaults for zero values that should have defaults
	if cfg.Auth.APIURL == "" {
		cfg.Auth.APIURL = "https://torrentclaw.com"
	}
	if cfg.Download.PreferredMethod == "" {
		cfg.Download.PreferredMethod = "auto"
	}
	if cfg.Download.MaxConcurrent == 0 {
		cfg.Download.MaxConcurrent = 3
	}
	if cfg.General.Country == "" {
		cfg.General.Country = "US"
	}

	return cfg, nil
}

// Save writes config to the default or specified path using atomic write.
func Save(cfg Config, path string) error {
	if path == "" {
		path = FilePath()
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	var buf strings.Builder
	encoder := toml.NewEncoder(&buf)
	if err := encoder.Encode(cfg); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}

	// Atomic write: write to temp, then rename
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(buf.String()), 0o600); err != nil {
		return fmt.Errorf("write temp config: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename config: %w", err)
	}

	return nil
}

// ParseSpeed parses a human-readable speed string into bytes/s.
// Supports: "10MB", "500KB", "1GB", "1024", "0" (unlimited).
func ParseSpeed(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}

	s = strings.ToUpper(s)
	multiplier := int64(1)

	switch {
	case strings.HasSuffix(s, "GB"):
		multiplier = 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "MB"):
		multiplier = 1024 * 1024
		s = strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "KB"):
		multiplier = 1024
		s = strings.TrimSuffix(s, "KB")
	}

	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid speed %q: %w", s, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("speed cannot be negative: %s", s)
	}

	return int64(n * float64(multiplier)), nil
}

// ApplyEnvOverrides applies UNARR_* environment variable overrides.
func (c *Config) ApplyEnvOverrides() {
	if v := os.Getenv("UNARR_API_KEY"); v != "" {
		c.Auth.APIKey = v
	}
	if v := os.Getenv("UNARR_API_URL"); v != "" {
		c.Auth.APIURL = v
	}
	if v := os.Getenv("UNARR_COUNTRY"); v != "" {
		c.General.Country = v
	}
	if v := os.Getenv("UNARR_DOWNLOAD_DIR"); v != "" {
		c.Download.Dir = v
	}
}

// dangerousPaths are system-critical directories that should never be used as
// download or organize targets (per platform).
var dangerousPaths = func() map[string]bool {
	m := map[string]bool{}
	// Unix
	for _, p := range []string{
		"/", "/bin", "/sbin", "/usr", "/lib", "/lib64", "/boot", "/dev", "/proc", "/sys",
		"/etc", "/var", "/tmp", "/root",
		// macOS
		"/System", "/Library", "/private", "/private/etc", "/private/tmp", "/private/var",
	} {
		m[p] = true
	}
	// Windows
	if runtime.GOOS == "windows" {
		for _, drive := range []string{"C", "D"} {
			for _, p := range []string{
				drive + `:\`,
				drive + `:\Windows`,
				drive + `:\Windows\System32`,
				drive + `:\Program Files`,
				drive + `:\Program Files (x86)`,
			} {
				m[filepath.Clean(p)] = true
			}
		}
	}
	return m
}()

// ValidatePaths checks that configured directories are safe to write to.
// Returns an error if any path points to a system directory or the user's
// home directory root (must use a subdirectory).
func (c *Config) ValidatePaths() error {
	home, _ := os.UserHomeDir()

	check := func(label, dir string) error {
		if dir == "" {
			return nil
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return fmt.Errorf("%s: invalid path %q: %w", label, dir, err)
		}
		clean := filepath.Clean(abs)

		if dangerousPaths[clean] {
			return fmt.Errorf("%s: refusing to use system directory %q", label, clean)
		}

		// Block home root — require a subdirectory
		if home != "" && clean == filepath.Clean(home) {
			return fmt.Errorf("%s: use a subdirectory of your home, not %q itself", label, clean)
		}

		// Block hidden dirs under home (e.g. ~/.ssh, ~/.gnupg)
		if home != "" && strings.HasPrefix(clean, filepath.Clean(home)+string(filepath.Separator)) {
			rel, _ := filepath.Rel(home, clean)
			first := strings.SplitN(rel, string(filepath.Separator), 2)[0]
			if strings.HasPrefix(first, ".") && first != ".local" && first != ".config" {
				return fmt.Errorf("%s: refusing to use hidden directory %q", label, clean)
			}
		}

		return nil
	}

	if err := check("downloads.dir", c.Download.Dir); err != nil {
		return err
	}
	if err := check("organize.movies_dir", c.Organize.MoviesDir); err != nil {
		return err
	}
	if err := check("organize.tv_shows_dir", c.Organize.TVShowsDir); err != nil {
		return err
	}
	return nil
}

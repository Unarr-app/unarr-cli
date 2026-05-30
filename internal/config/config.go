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
	// Mirrors lists alternate base URLs the agent will fall back to when the
	// primary api_url is unreachable. Ordered by preference. Refreshed at
	// runtime by `unarr mirrors update` against /api/v1/mirrors so a long-
	// running agent survives a primary takedown without a new release.
	Mirrors []string `toml:"mirrors"`
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
	StreamPort       int    `toml:"stream_port"`        // fixed port for streaming HTTP server (default: 11818)
	EnableUPnP       bool   `toml:"enable_upnp"`        // map StreamPort to the WAN via UPnP/NAT-PMP (default: false; opt-in)
	// RequireStreamToken gates remote (non-loopback) /stream + /hls requests on a
	// signed, short-lived token embedded in the URLs the agent reports. Default
	// true (secure by default); loopback callers (local mpv/vlc) are always exempt.
	// Set false only to debug a player that can't carry the token.
	RequireStreamToken bool            `toml:"require_stream_token"`
	CORSExtraOrigins   []string        `toml:"cors_extra_origins"` // extra browser origins added on top of the baked-in allowlist (torrentclaw.com, app.torrentclaw.com, localhost:3030)
	Transcode          TranscodeConfig `toml:"transcode"`
	HLSCache           HLSCacheConfig  `toml:"hls_cache"`
	VPN                VPNConfig       `toml:"vpn"`
	Funnel             FunnelConfig    `toml:"funnel"`
}

// HLSCacheConfig controls the persistent HLS segment cache. A completed encode
// is kept on disk so a second play of the same file at the same quality skips
// ffmpeg entirely. Old entries are evicted (LRU) once the cache exceeds the
// size budget. Enabled by default — disable to save disk space at the cost of
// re-encoding every play.
type HLSCacheConfig struct {
	Enabled bool   `toml:"enabled"` // default: true
	SizeGB  int    `toml:"size_gb"` // size budget in gigabytes; default: 5; minimum: 1
	Dir     string `toml:"dir"`     // override storage path; default: ~/.cache/unarr/hls-cache
}

// FunnelConfig gates the optional CloudFlare Quick Tunnel that exposes the
// daemon's HLS server over a public HTTPS hostname (https://<random>.try
// cloudflare.com). Enabling it lets the web player on torrentclaw.com play
// from this daemon across any network without Tailscale or a public IP —
// the cost is that bytes proxy through CloudFlare's network. Off by default.
type FunnelConfig struct {
	Enabled bool `toml:"enabled"`
}

// VPNConfig gates the managed-VPN add-on split-tunnel. When enabled, the daemon
// fetches a WireGuard config from the web (/api/internal/agent/vpn-config) and
// routes only the torrent client's peer/tracker traffic through an in-process
// userspace tunnel (no root, no OS routing changes). Requires an active VPN
// add-on on the account; otherwise the daemon logs and downloads in the clear.
type VPNConfig struct {
	Enabled bool `toml:"enabled"`
	// ConfigFile, when set, makes the daemon read a local WireGuard .conf instead
	// of fetching one from the web API. For self-hosted / personal-VPN testing:
	// point it at a peer .conf from your own WireGuard server and the torrent
	// client split-tunnels through it with no web/provider plumbing.
	ConfigFile string `toml:"config_file"`
}

// TranscodeConfig controls real-time transcoding for the in-browser player
// when source codecs aren't browser-decodable (HEVC, AV1, AC3, DTS, etc.).
// Disabled by default; enabling requires ffmpeg + ffprobe on PATH (or
// explicit paths via the library config).
type TranscodeConfig struct {
	Enabled bool   `toml:"enabled"`  // master switch
	HWAccel string `toml:"hw_accel"` // "auto" | "none" | "nvenc" | "qsv" | "vaapi" | "videotoolbox"
	// Preset is the encoder speed/quality dial. Only used on software encode
	// (libx264) — HW backends (NVENC/QSV/VAAPI/VideoToolbox) use vendor
	// presets that don't share libx264's vocabulary and would be rejected
	// by ffmpeg if passed here.
	//
	// Empty (default) → engine picks "superfast" — latency-biased, ~3 s
	// first-play on 1080p source on a modern x86 CPU. Marginal quality loss
	// at 5-25 Mbps target bitrates.
	//
	// For better quality at slower first-play (1-2 s slower per seg):
	//   "veryfast"  — previous default; balanced
	//   "faster"    — slight quality bump
	//   "fast"      — meaningful quality bump
	//   "medium"    — libx264 stock default; CPU-bound on 4K
	//   "slow" / "slower" / "veryslow" — only for batch encodes, not real-time HLS
	//
	// Or faster:
	//   "ultrafast" — lowest quality, fastest encode
	Preset        string `toml:"preset"`
	VideoBitrate  string `toml:"video_bitrate"`  // e.g. "5M"
	AudioBitrate  string `toml:"audio_bitrate"`  // e.g. "192k"
	MaxHeight     int    `toml:"max_height"`     // optional downscale cap (e.g. 720)
	MaxConcurrent int    `toml:"max_concurrent"` // safety cap on simultaneous transcoder processes
}

type OrganizeConfig struct {
	Enabled    bool   `toml:"enabled"`
	MoviesDir  string `toml:"movies_dir"`
	TVShowsDir string `toml:"tv_shows_dir"`
}

type DaemonConfig struct {
	StatusInterval string `toml:"status_interval"`
	// AutoUpgrade gates the daemon's response to a server-flagged upgrade
	// (set via the "Force update" button on the web). When true the daemon
	// downloads + replaces the binary in-place and exits so the service
	// supervisor respawns on the new version. When false the daemon only
	// logs "new version available" and the operator must run `unarr update`
	// manually. Default: true. Available since unarr 0.9.6.
	AutoUpgrade *bool `toml:"auto_upgrade"`
}

// AutoUpgradeEnabled returns the resolved AutoUpgrade flag — defaults to true
// when the user has not set it explicitly. Pointer-vs-bool because Go's
// zero-value bool would collapse "unset" and "false" together.
func (d DaemonConfig) AutoUpgradeEnabled() bool {
	if d.AutoUpgrade == nil {
		return true
	}
	return *d.AutoUpgrade
}

func boolPtr(v bool) *bool { return &v }

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
	FFmpegPath   string `toml:"ffmpeg_path"`   // optional explicit path (used by the HLS streaming transcoder)
	BackupDir    string `toml:"backup_dir"`    // for replaced files
	AutoScan     bool   `toml:"auto_scan"`     // enable daily auto-scan in daemon (default true)
	ScanInterval string `toml:"scan_interval"` // e.g. "24h", "12h", "6h" (default "24h")
	AllowDelete  bool   `toml:"allow_delete"`  // allow web UI to request file deletion from disk
}

// Default returns a Config with sensible defaults. Used both for fresh
// installs (no config file yet) and as the baseline for Load — fields not
// present in the user's TOML keep their Default() value.
func Default() Config {
	return Config{
		Auth: AuthConfig{
			APIURL: "https://torrentclaw.com",
			// Default mirror list. Kept in sync with src/lib/mirrors-config.ts
			// on the server. Users can override with `unarr mirrors update`,
			// which pulls the live list from /api/v1/mirrors.
			Mirrors: []string{
				"https://torrentclaw.to",
			},
		},
		Download: DownloadConfig{
			PreferredMethod:    "auto",
			MaxConcurrent:      3,
			StreamPort:         11818,
			RequireStreamToken: true, // secure by default; loopback exempt
			Transcode: TranscodeConfig{
				Enabled: true,
				HWAccel: "auto",
				// Empty preset → engine.ResolveEncoderProfile picks the
				// latency-biased default ("superfast" on libx264). Override
				// in config.toml when quality > first-start latency matters.
				Preset:        "",
				AudioBitrate:  "192k",
				MaxConcurrent: 2,
			},
			Funnel: FunnelConfig{
				// On by default so headless installs (NAS / Docker) get cross-network
				// HTTPS playback without anyone having to terminal in. Users who
				// don't want bytes proxied through CloudFlare can opt out with
				// `unarr funnel off` (sets enabled=false in the TOML).
				Enabled: true,
			},
			HLSCache: HLSCacheConfig{
				// On by default — second play of a recently watched file at the
				// same quality skips ffmpeg (instant start, near-zero CPU).
				// Users can opt out (hls_cache.enabled=false) or shrink the
				// budget (hls_cache.size_gb) when disk is tight.
				Enabled: true,
				SizeGB:  5,
			},
		},
		Daemon: DaemonConfig{
			// Pointer-to-true so Default() round-trips through TOML marshal
			// as `auto_upgrade = true` instead of an omitted key — keeps the
			// freshly-written config aligned with what README documents.
			AutoUpgrade: boolPtr(true),
		},
		Organize: OrganizeConfig{
			Enabled: true,
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

	meta, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}

	applyDefaults(&cfg, meta)
	return cfg, nil
}

// applyDefaults fills in sensible defaults for keys that the user did not
// define in the TOML file. We use MetaData (rather than zero-value checks) so
// that explicitly setting a field to its zero value (e.g. `enabled = false`)
// is respected — only truly missing keys get defaulted. This lets a fresh
// install work out of the box for streaming without forcing every user to
// edit the TOML, while still letting power users disable features.
func applyDefaults(cfg *Config, meta toml.MetaData) {
	if !meta.IsDefined("auth", "api_url") {
		cfg.Auth.APIURL = "https://torrentclaw.com"
	}
	if !meta.IsDefined("auth", "mirrors") {
		cfg.Auth.Mirrors = []string{"https://torrentclaw.to"}
	}
	if !meta.IsDefined("downloads", "preferred_method") {
		cfg.Download.PreferredMethod = "auto"
	}
	if !meta.IsDefined("downloads", "max_concurrent") {
		cfg.Download.MaxConcurrent = 3
	}
	if !meta.IsDefined("downloads", "stream_port") {
		cfg.Download.StreamPort = 11818
	}
	if !meta.IsDefined("general", "country") {
		cfg.General.Country = "US"
	}

	if !meta.IsDefined("downloads", "transcode", "enabled") {
		cfg.Download.Transcode.Enabled = true
	}
	if !meta.IsDefined("downloads", "transcode", "hw_accel") {
		cfg.Download.Transcode.HWAccel = "auto"
	}
	if !meta.IsDefined("downloads", "transcode", "preset") {
		// Empty = let engine.ResolveEncoderProfile pick the latency-biased
		// default ("superfast" on libx264). Users wanting better quality at
		// slower first-play can override to "veryfast" / "fast" / "medium" in
		// config.toml. Ignored when hw_accel picks NVENC/QSV/VAAPI/VideoToolbox
		// (those have built-in vendor presets).
		cfg.Download.Transcode.Preset = ""
	}
	if !meta.IsDefined("downloads", "transcode", "audio_bitrate") {
		cfg.Download.Transcode.AudioBitrate = "192k"
	}
	if !meta.IsDefined("downloads", "transcode", "max_concurrent") {
		cfg.Download.Transcode.MaxConcurrent = 2
	}
	// NOTE: Funnel default-ON only applies to fresh installs (no config file →
	// Default() returns Funnel.Enabled=true straight off). When an existing
	// config file lacks `[downloads.funnel]` entirely we intentionally do NOT
	// flip it on here — that would silently route an upgraded operator's
	// traffic through CloudFlare without their consent. They opt in with
	// `unarr funnel on` whenever they're ready.
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

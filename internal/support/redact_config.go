package support

import (
	"bytes"

	"github.com/BurntSushi/toml"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

// publishedConfig is the ONLY shape that reaches config.redacted.toml.
//
// It is a separate type, not a filtered copy of config.Config, and that is the
// entire security design: a new field in config.Config is not in this struct,
// so it is not serialised, so it cannot leak. The opposite arrangement —
// copying the Config and blanking the fields we know are secret — inverts the
// default, and the day someone adds a credential we forgot to blank is the day
// a user pastes it into a public issue.
//
// Every string here is a value produced by redact_values.go, never a value
// taken from the user's file. See the comment there for why.
type publishedConfig struct {
	Auth          publishedAuth          `toml:"auth"`
	Agent         publishedAgent         `toml:"agent"`
	Downloads     publishedDownloads     `toml:"downloads"`
	Organize      publishedOrganize      `toml:"organize"`
	Daemon        publishedDaemon        `toml:"daemon"`
	Notifications publishedNotifications `toml:"notifications"`
	General       publishedGeneral       `toml:"general"`
	Telemetry     publishedTelemetry     `toml:"telemetry"`
	Desktop       publishedDesktop       `toml:"desktop"`
	Library       publishedLibrary       `toml:"library"`
}

type publishedAuth struct {
	APIKey  string `toml:"api_key"` // always the withheld marker; see redactConfig
	APIURL  string `toml:"api_url"`
	Mirrors string `toml:"mirrors"`
}

type publishedAgent struct {
	ID   string `toml:"id"`
	Name string `toml:"name"`
	Hash string `toml:"agent_hash"`
}

type publishedDownloads struct {
	Dir                string   `toml:"dir"`
	PreferredMethod    string   `toml:"preferred_method"`
	PreferredMethods   []string `toml:"preferred_methods"`
	PreferredQuality   string   `toml:"preferred_quality"`
	MaxConcurrent      int      `toml:"max_concurrent"`
	MinFreeDiskMB      int      `toml:"min_free_disk_mb"`
	MaxDownloadSpeed   string   `toml:"max_download_speed"`
	MaxUploadSpeed     string   `toml:"max_upload_speed"`
	SeedEnabled        bool     `toml:"seed_enabled"`
	SeedRatio          float64  `toml:"seed_ratio"`
	SeedTime           string   `toml:"seed_time"`
	MetadataTimeout    string   `toml:"metadata_timeout"`
	StallTimeout       string   `toml:"stall_timeout"`
	ListenPort         int      `toml:"listen_port"`
	StreamPort         int      `toml:"stream_port"`
	HTTPSStreamPort    int      `toml:"https_stream_port"`
	EnableUPnP         bool     `toml:"enable_upnp"`
	AutoHTTPSUpnp      bool     `toml:"auto_https_upnp"`
	MaxStreamSessions  int      `toml:"max_stream_sessions"`
	UsenetStreaming    bool     `toml:"usenet_streaming"`
	RequireStreamToken bool     `toml:"require_stream_token"`
	CORSExtraOrigins   string   `toml:"cors_extra_origins"`
	WebDAVEnabled      bool     `toml:"webdav_enabled"`
	WebDAVAllowWAN     bool     `toml:"webdav_allow_wan"`
	WebDAVUsername     string   `toml:"webdav_username"`
	WebDAVPassword     string   `toml:"webdav_password"`

	// Sub-tables come LAST: the TOML encoder writes fields in declaration
	// order, and a scalar emitted after a table header would be parsed back as
	// belonging to that table.
	Transcode publishedTranscode `toml:"transcode"`
	HLSCache  publishedHLSCache  `toml:"hls_cache"`
	VPN       publishedVPN       `toml:"vpn"`
	Funnel    publishedFunnel    `toml:"funnel"`
}

type publishedTranscode struct {
	Enabled       bool   `toml:"enabled"`
	HWAccel       string `toml:"hw_accel"`
	Preset        string `toml:"preset"`
	VideoBitrate  string `toml:"video_bitrate"`
	AudioBitrate  string `toml:"audio_bitrate"`
	MaxHeight     int    `toml:"max_height"`
	MaxConcurrent int    `toml:"max_concurrent"`
}

type publishedHLSCache struct {
	Enabled bool   `toml:"enabled"`
	SizeGB  int    `toml:"size_gb"`
	Dir     string `toml:"dir"`
}

type publishedVPN struct {
	Enabled    bool   `toml:"enabled"`
	Required   bool   `toml:"required"`
	ConfigFile string `toml:"config_file"`
}

type publishedFunnel struct {
	Enabled bool `toml:"enabled"`
}

type publishedOrganize struct {
	Enabled    bool   `toml:"enabled"`
	MoviesDir  string `toml:"movies_dir"`
	TVShowsDir string `toml:"tv_shows_dir"`
}

type publishedDaemon struct {
	StatusInterval string `toml:"status_interval"`
	AutoUpgrade    string `toml:"auto_upgrade"`
	Downlink       string `toml:"downlink"`
	LogMaxSizeMB   int    `toml:"log_max_size_mb"`
	LogMaxFiles    int    `toml:"log_max_files"`
	LogLevel       string `toml:"log_level"`
	LogFormat      string `toml:"log_format"`
}

type publishedNotifications struct {
	Enabled bool `toml:"enabled"`
}

type publishedGeneral struct {
	Country string `toml:"country"`
	Locale  string `toml:"locale"`
	NoColor bool   `toml:"no_color"`
}

type publishedTelemetry struct {
	Enabled string `toml:"enabled"`
}

type publishedDesktop struct {
	Player        string `toml:"player"`
	PlayerCommand string `toml:"player_command"`
}

type publishedLibrary struct {
	ScanPath            string  `toml:"scan_path"`
	Workers             int     `toml:"workers"`
	FFprobePath         string  `toml:"ffprobe_path"`
	FFmpegPath          string  `toml:"ffmpeg_path"`
	BackupDir           string  `toml:"backup_dir"`
	AutoScan            bool    `toml:"auto_scan"`
	ScanInterval        string  `toml:"scan_interval"`
	AllowDelete         bool    `toml:"allow_delete"`
	CacheSubtitles      bool    `toml:"cache_subtitles"`
	CacheThumbnails     bool    `toml:"cache_thumbnails"`
	SkipDetect          bool    `toml:"skip_detect"`
	PrewarmMaxLoadRatio float64 `toml:"prewarm_max_load_ratio"`

	// Sub-tables last; see publishedDownloads.
	Trickplay publishedTrickplay `toml:"trickplay"`
	Subtitles publishedSubtitles `toml:"subtitles"`
	Cleanup   publishedCleanup   `toml:"cleanup"`
}

type publishedTrickplay struct {
	Enabled  bool   `toml:"enabled"`
	Interval string `toml:"interval"`
	Width    int    `toml:"width"`
}

type publishedSubtitles struct {
	AutoFetch bool     `toml:"auto_fetch"`
	Languages []string `toml:"languages"`
}

type publishedCleanup struct {
	Enabled               bool   `toml:"enabled"`
	MinVideoBytes         string `toml:"min_video_bytes"`
	RemoveStubs           bool   `toml:"remove_stubs"`
	RemoveOrphanPartials  bool   `toml:"remove_orphan_partials"`
	DedupExact            bool   `toml:"dedup_exact"`
	RemoveOrphanSubtitles bool   `toml:"remove_orphan_subtitles"`
	PruneEmptyDirs        bool   `toml:"prune_empty_dirs"`
	RemoveCorruptVideos   bool   `toml:"remove_corrupt_videos"`
}

// withheld is what a Secret field renders as. It is emitted rather than
// omitted on purpose: "api_key = <withheld>" tells the reader the key IS set,
// which is a real diagnostic, while an absent line would be ambiguous with a
// blank one.
const withheld = "<withheld>"

// withheldOrUnset distinguishes "set, and we are not telling you" from "never
// configured" — the second is a common root cause and the first must not hide it.
func withheldOrUnset(v string) string {
	if v == "" {
		return valueUnset
	}
	return withheld
}

// Vocabularies. Kept next to the builder so a reviewer can check them against
// the doc comments in config.go without changing files.
var (
	methodVocab   = []string{"auto", "debrid", "usenet", "torrent"}
	qualityVocab  = []string{"2160p", "1080p", "720p", "480p", "auto", "best"}
	hwAccelVocab  = []string{"auto", "none", "nvenc", "qsv", "vaapi", "videotoolbox", "amf", "v4l2m2m"}
	presetVocab   = []string{"ultrafast", "superfast", "veryfast", "faster", "fast", "medium", "slow", "slower", "veryslow", "placebo"}
	logLevelVocab = []string{"debug", "info", "warn", "warning", "error"}
	logFmtVocab   = []string{"text", "plain", "console", "json", "jsonl", "json-lines", "ndjson"}
	downlinkVocab = []string{"auto", "sse", "poll"}
	playerVocab   = []string{"auto", "vlc", "mpv", "iina", "infuse", "custom", "system"}
)

// redactConfig projects the live Config onto the publishable shape. Nothing in
// here reads a field that is not classified in configFields; the reflection
// test is what keeps that true as the schema grows.
func redactConfig(c config.Config) publishedConfig {
	return publishedConfig{
		Auth: publishedAuth{
			APIKey:  withheldOrUnset(c.Auth.APIKey),
			APIURL:  endpoint(c.Auth.APIURL),
			Mirrors: count(c.Auth.Mirrors),
		},
		Agent: publishedAgent{
			ID:   presence(c.Agent.ID),
			Name: presence(c.Agent.Name),
			Hash: withheldOrUnset(c.Agent.Hash),
		},
		Downloads:     redactDownloads(c.Download),
		Organize:      publishedOrganize{Enabled: c.Organize.Enabled, MoviesDir: presence(c.Organize.MoviesDir), TVShowsDir: presence(c.Organize.TVShowsDir)},
		Daemon:        redactDaemon(c.Daemon),
		Notifications: publishedNotifications{Enabled: c.Notifications.Enabled},
		General:       publishedGeneral{Country: shaped(c.General.Country, regionShape), Locale: shaped(c.General.Locale, langShape), NoColor: c.General.NoColor},
		Telemetry:     publishedTelemetry{Enabled: tribool(c.Telemetry.Enabled)},
		Desktop:       publishedDesktop{Player: pick(c.Desktop.Player, playerVocab...), PlayerCommand: presence(c.Desktop.PlayerCommand)},
		Library:       redactLibrary(c.Library),
	}
}

func redactDownloads(d config.DownloadConfig) publishedDownloads {
	return publishedDownloads{
		Dir:                presence(d.Dir),
		PreferredMethod:    pick(d.PreferredMethod, methodVocab...),
		PreferredMethods:   picks(d.PreferredMethods, methodVocab...),
		PreferredQuality:   pick(d.PreferredQuality, qualityVocab...),
		MaxConcurrent:      d.MaxConcurrent,
		MinFreeDiskMB:      d.MinFreeDiskMB,
		MaxDownloadSpeed:   shaped(d.MaxDownloadSpeed, sizeShape),
		MaxUploadSpeed:     shaped(d.MaxUploadSpeed, sizeShape),
		SeedEnabled:        d.SeedEnabled,
		SeedRatio:          d.SeedRatio,
		SeedTime:           shaped(d.SeedTime, durationShape),
		MetadataTimeout:    shaped(d.MetadataTimeout, durationShape),
		StallTimeout:       shaped(d.StallTimeout, durationShape),
		ListenPort:         d.ListenPort,
		StreamPort:         d.StreamPort,
		HTTPSStreamPort:    d.HTTPSStreamPort,
		EnableUPnP:         d.EnableUPnP,
		AutoHTTPSUpnp:      d.AutoHTTPSUpnp,
		MaxStreamSessions:  d.MaxStreamSessions,
		UsenetStreaming:    d.UsenetStreaming,
		RequireStreamToken: d.RequireStreamToken,
		CORSExtraOrigins:   count(d.CORSExtraOrigins),
		WebDAVEnabled:      d.WebDAVEnabled,
		WebDAVAllowWAN:     d.WebDAVAllowWAN,
		WebDAVUsername:     withheldOrUnset(d.WebDAVUsername),
		WebDAVPassword:     withheldOrUnset(d.WebDAVPassword),
		Transcode:          redactTranscode(d.Transcode),
		HLSCache:           publishedHLSCache{Enabled: d.HLSCache.Enabled, SizeGB: d.HLSCache.SizeGB, Dir: presence(d.HLSCache.Dir)},
		VPN:                publishedVPN{Enabled: d.VPN.Enabled, Required: d.VPN.Required, ConfigFile: presence(d.VPN.ConfigFile)},
		Funnel:             publishedFunnel{Enabled: d.Funnel.Enabled},
	}
}

func redactTranscode(t config.TranscodeConfig) publishedTranscode {
	return publishedTranscode{
		Enabled:       t.Enabled,
		HWAccel:       pick(t.HWAccel, hwAccelVocab...),
		Preset:        pick(t.Preset, presetVocab...),
		VideoBitrate:  shaped(t.VideoBitrate, bitrateShape),
		AudioBitrate:  shaped(t.AudioBitrate, bitrateShape),
		MaxHeight:     t.MaxHeight,
		MaxConcurrent: t.MaxConcurrent,
	}
}

func redactDaemon(d config.DaemonConfig) publishedDaemon {
	return publishedDaemon{
		StatusInterval: shaped(d.StatusInterval, durationShape),
		AutoUpgrade:    tribool(d.AutoUpgrade),
		Downlink:       pick(d.Downlink, downlinkVocab...),
		LogMaxSizeMB:   d.LogMaxSizeMB,
		LogMaxFiles:    d.LogMaxFiles,
		LogLevel:       pick(d.LogLevel, logLevelVocab...),
		LogFormat:      pick(d.LogFormat, logFmtVocab...),
	}
}

func redactLibrary(l config.LibraryConfig) publishedLibrary {
	return publishedLibrary{
		ScanPath:            presence(l.ScanPath),
		Workers:             l.Workers,
		FFprobePath:         presence(l.FFprobePath),
		FFmpegPath:          presence(l.FFmpegPath),
		BackupDir:           presence(l.BackupDir),
		AutoScan:            l.AutoScan,
		ScanInterval:        shaped(l.ScanInterval, durationShape),
		AllowDelete:         l.AllowDelete,
		CacheSubtitles:      l.CacheSubtitles,
		CacheThumbnails:     l.CacheThumbnails,
		SkipDetect:          l.SkipDetect,
		PrewarmMaxLoadRatio: l.PrewarmMaxLoadRatio,
		Trickplay:           publishedTrickplay{Enabled: l.Trickplay.Enabled, Interval: shaped(l.Trickplay.Interval, durationShape), Width: l.Trickplay.Width},
		Subtitles:           publishedSubtitles{AutoFetch: l.Subtitles.AutoFetch, Languages: shapedList(l.Subtitles.Languages, langShape)},
		Cleanup:             redactCleanup(l.Cleanup),
	}
}

func redactCleanup(c config.CleanupConfig) publishedCleanup {
	return publishedCleanup{
		Enabled:               c.Enabled,
		MinVideoBytes:         shaped(c.MinVideoBytes, sizeShape),
		RemoveStubs:           c.RemoveStubs,
		RemoveOrphanPartials:  c.RemoveOrphanPartials,
		DedupExact:            c.DedupExact,
		RemoveOrphanSubtitles: c.RemoveOrphanSubtitles,
		PruneEmptyDirs:        c.PruneEmptyDirs,
		RemoveCorruptVideos:   c.RemoveCorruptVideos,
	}
}

// configTOML renders the publishable projection. The header is part of the
// artefact: whoever opens this file in an issue thread should not have to
// guess whether the blanks are the user's or ours.
func configTOML(c config.Config) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("# unarr support bundle — redacted configuration.\n" +
		"#\n" +
		"# This is NOT your config.toml. It is a projection built from an explicit\n" +
		"# allowlist: credentials are withheld, and free-form values (paths, names,\n" +
		"# custom URLs) are reported as <set>/<unset> or <custom> rather than copied.\n" +
		"# A value shown as <non-standard> is set to something unarr does not\n" +
		"# recognise for that key.\n\n")
	if err := toml.NewEncoder(&buf).Encode(redactConfig(c)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

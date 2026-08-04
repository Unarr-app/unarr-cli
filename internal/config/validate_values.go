package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ValueIssues reports configured values that are outside their accepted range
// or drawn from an unknown vocabulary — the other half of "silently ignored"
// config, next to UnknownKeyIssues. Pure: no filesystem, no network. Path
// SAFETY is deliberately not checked here (that is ValidatePaths, which needs
// the home dir), so this stays testable without I/O.
//
// Deliberately lenient about anything the loader documents as self-healing
// (max_stream_sessions <= 0 → 1, cleanup.min_video_bytes → 1 MiB): warning
// about a value that already has a defined fallback would be noise.
func (c *Config) ValueIssues() []Issue {
	var l issueList
	l = append(l, c.downloadValueIssues()...)
	l = append(l, c.libraryValueIssues()...)
	l = append(l, c.miscValueIssues()...)
	return l
}

// issueList accumulates findings so each individual check reads as one line
// instead of an if/append pair.
type issueList []Issue

// add records a value problem for key.
func (l *issueList) add(key, detail string) {
	*l = append(*l, Issue{Key: key, Message: fmt.Sprintf("invalid value for %q: %s", key, detail)})
}

// duration flags a non-empty duration string time.ParseDuration rejects. "0" is
// accepted: every duration key in the schema spells "unlimited" that way.
func (l *issueList) duration(key, v string) {
	v = strings.TrimSpace(v)
	if v == "" || v == "0" {
		return
	}
	if _, err := time.ParseDuration(v); err != nil {
		l.add(key, fmt.Sprintf("%q is not a duration (e.g. \"30m\", \"1h\", \"0\" = unlimited)", v))
	}
}

// port flags a value outside the TCP port range. 0 is valid everywhere it means
// "random" or "disabled".
func (l *issueList) port(key string, v int) {
	if v < 0 || v > 65535 {
		l.add(key, fmt.Sprintf("%d is not a TCP port (0-65535, 0 = disabled)", v))
	}
}

// nonNegative flags a count that cannot be negative.
func (l *issueList) nonNegative(key string, v int) {
	if v < 0 {
		l.add(key, fmt.Sprintf("%d must be zero or positive", v))
	}
}

// oneOf flags a non-empty string outside a closed vocabulary. Matching is
// case- and whitespace-insensitive, mirroring how the consumers normalise.
func (l *issueList) oneOf(key, v string, allowed ...string) {
	norm := strings.ToLower(strings.TrimSpace(v))
	if norm == "" || contains(allowed, norm) {
		return
	}
	l.add(key, fmt.Sprintf("%q is not one of: %s", v, strings.Join(allowed, ", ")))
}

// method flags a download backend name MethodOrder() would silently drop.
func (l *issueList) method(key, v string) {
	norm := strings.ToLower(strings.TrimSpace(v))
	if norm == "" || norm == "auto" || validMethod(norm) {
		return
	}
	l.add(key, fmt.Sprintf("%q is not one of: auto, torrent, debrid, usenet", v))
}

// speed flags a rate limit ParseSpeed cannot read.
func (l *issueList) speed(key, v string) {
	if strings.TrimSpace(v) == "" {
		return
	}
	if _, err := ParseSpeed(v); err != nil {
		l.add(key, fmt.Sprintf("%q is not a speed (e.g. \"10MB\", \"500KB\", \"0\" = unlimited)", v))
	}
}

// httpURL flags a base URL that is not an absolute http(s) URL — the shape
// every API call assumes.
func (l *issueList) httpURL(key, v string) {
	if strings.TrimSpace(v) == "" {
		return
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		l.add(key, fmt.Sprintf("%q is not an http(s) URL", v))
	}
}

func (c *Config) downloadValueIssues() issueList {
	var l issueList
	d := c.Download
	l.nonNegative("downloads.max_concurrent", d.MaxConcurrent)
	l.nonNegative("downloads.min_free_disk_mb", d.MinFreeDiskMB)
	l.port("downloads.listen_port", d.ListenPort)
	l.port("downloads.stream_port", d.StreamPort)
	l.port("downloads.https_stream_port", d.HTTPSStreamPort)
	l.speed("downloads.max_download_speed", d.MaxDownloadSpeed)
	l.speed("downloads.max_upload_speed", d.MaxUploadSpeed)
	l.duration("downloads.seed_time", d.SeedTime)
	l.duration("downloads.metadata_timeout", d.MetadataTimeout)
	l.duration("downloads.stall_timeout", d.StallTimeout)
	l.oneOf("downloads.preferred_quality", d.PreferredQuality, "2160p", "1080p", "720p", "480p")
	l.method("downloads.preferred_method", d.PreferredMethod)
	for _, m := range d.PreferredMethods {
		l.method("downloads.preferred_methods", m)
	}
	if d.SeedRatio < 0 {
		l.add("downloads.seed_ratio", fmt.Sprintf("%g must be zero or positive", d.SeedRatio))
	}
	l.nonNegative("downloads.hls_cache.size_gb", d.HLSCache.SizeGB)
	l = append(l, c.transcodeValueIssues()...)
	return l
}

func (c *Config) transcodeValueIssues() issueList {
	var l issueList
	t := c.Download.Transcode
	l.oneOf("downloads.transcode.hw_accel", t.HWAccel,
		"auto", "none", "nvenc", "qsv", "vaapi", "videotoolbox")
	// libx264 preset vocabulary — only consulted on software encode, but a typo
	// here would be rejected by ffmpeg at play time, which is far too late.
	l.oneOf("downloads.transcode.preset", t.Preset,
		"ultrafast", "superfast", "veryfast", "faster", "fast",
		"medium", "slow", "slower", "veryslow")
	l.nonNegative("downloads.transcode.max_height", t.MaxHeight)
	l.nonNegative("downloads.transcode.max_concurrent", t.MaxConcurrent)
	return l
}

func (c *Config) libraryValueIssues() issueList {
	var l issueList
	lib := c.Library
	l.nonNegative("library.workers", lib.Workers)
	l.duration("library.scan_interval", lib.ScanInterval)
	l.duration("library.trickplay.interval", lib.Trickplay.Interval)
	l.nonNegative("library.trickplay.width", lib.Trickplay.Width)
	if lib.PrewarmMaxLoadRatio < 0 {
		l.add("library.prewarm_max_load_ratio",
			fmt.Sprintf("%g must be zero or positive (0 = use the default)", lib.PrewarmMaxLoadRatio))
	}
	return l
}

func (c *Config) miscValueIssues() issueList {
	var l issueList
	l.httpURL("auth.api_url", c.Auth.APIURL)
	for _, m := range c.Auth.Mirrors {
		l.httpURL("auth.mirrors", m)
	}
	l.duration("daemon.status_interval", c.Daemon.StatusInterval)
	l.oneOf("daemon.downlink", c.Daemon.Downlink, "auto", "sse", "poll")
	l.nonNegative("daemon.log_max_size_mb", c.Daemon.LogMaxSizeMB)
	l.nonNegative("daemon.log_max_files", c.Daemon.LogMaxFiles)
	l.oneOf("daemon.log_level", c.Daemon.LogLevel, "debug", "info", "warn", "error")
	l.oneOf("daemon.log_format", c.Daemon.LogFormat, "text", "json")
	l.oneOf("desktop.player", c.Desktop.Player, "mpv", "vlc", "iina", "mpc", "system")
	return l
}

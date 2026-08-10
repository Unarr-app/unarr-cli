package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/Unarr-app/unarr-cli/internal/logging"
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
	Telemetry     TelemetryConfig     `toml:"telemetry,omitempty"`
	Desktop       DesktopConfig       `toml:"desktop,omitempty"`

	// unknownKeys holds the dotted TOML keys Load could not map onto the schema
	// (a typo, or a valid key under the wrong section). UNEXPORTED on purpose:
	// both the TOML decoder and encoder skip unexported fields, so carrying the
	// diagnostic on Config costs no call-site churn and Save never writes it
	// back into the user's file. Read it through UnknownKeys() / the Issue
	// helpers in validate.go.
	unknownKeys []string
}

// DesktopConfig holds settings for the unarr-desktop tray / unarr:// protocol
// handler companion. It lives in the daemon's config.toml (one file, one
// mental model for users) but the daemon itself never reads it. `omitempty`
// is load-bearing: BurntSushi/toml omits zero-value structs on encode, so
// daemon-side Load→Save round-trips (config menu, `unarr funnel off`, …)
// neither invent an empty [desktop] section in configs that never set it nor
// drop a user's setting.
type DesktopConfig struct {
	// Player forces which LOCAL media player `unarr-desktop --open` dispatches
	// to: "mpv" (also resolves Celluloid, the GTK front-end that embeds mpv and
	// installs no `mpv` binary) | "vlc" | "iina" (macOS only) | "mpc" (Windows
	// only) | "system" (whatever the OS opens video with). Empty = autodetect
	// (mpv > vlc > iina > mpc, then the OS default).
	// A configured player that isn't installed falls back to autodetect AND
	// notifies, so the setting never looks silently ignored.
	// "web" was accepted here until the browser stopped being a selectable
	// player — playing in the browser is the WEB's own button, not a second
	// route through the desktop. Such a config now autodetects (with a notice).
	// The UNARR_DESKTOP_PLAYER env var
	// overrides this — checked by the desktop binary directly, since
	// config.Load() deliberately does not apply env overrides.
	Player string `toml:"player,omitempty"`
	// PlayerCommand is an explicit command line, for players no name in the
	// list can reach: a Flatpak/Snap/AppImage (nothing on PATH), mpv.net,
	// SMPlayer, or just a player invoked with flags of your own. It WINS over
	// Player. Placeholders: {url} {web} {start} {title} {alang} {slang} — a
	// token whose placeholder has no value for a given stream is dropped, so
	// `--start={start}` simply disappears when there is nothing to resume.
	// Split into arguments here and exec'd directly, never through a shell.
	//
	//	player_command = "flatpak run org.videolan.VLC --start-time={start} -- {url}"
	//
	// Env override: UNARR_DESKTOP_PLAYER_COMMAND.
	PlayerCommand string `toml:"player_command,omitempty"`
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
	// Hash is a stable high-entropy label (hex) for the per-agent direct-TLS
	// feature. Distinct from ID (a UUID that could be guessed/enumerated): the
	// cert broker issues *.<hash>.agent.unarr.app and the web encodes the agent's
	// IP into a hostname under that wildcard. Generated + persisted on first run.
	Hash string `toml:"agent_hash,omitempty"`
}

type DownloadConfig struct {
	Dir string `toml:"dir"`
	// PreferredMethod (singular, legacy) — kept for back-compat. A single
	// "auto"|"torrent"|"debrid"|"usenet". Superseded by PreferredMethods.
	PreferredMethod string `toml:"preferred_method"`
	// PreferredMethods (ordered list) is the source of truth when set, e.g.
	// ["debrid","usenet"] = try debrid, then usenet, and DISABLE torrent (it's
	// not in the list). ["auto"] or empty → defer to the web policy. The web
	// honours this (reported on register) so a "debrid only" agent never gets a
	// torrent task it didn't ask for. See MethodOrder() for resolution.
	PreferredMethods []string `toml:"preferred_methods"`
	PreferredQuality string   `toml:"preferred_quality"` // "2160p", "1080p", "720p" — hint for auto-selection
	MaxConcurrent    int      `toml:"max_concurrent"`
	MinFreeDiskMB    int      `toml:"min_free_disk_mb"`   // refuse a download if it would leave less than this free (reserve to keep the FS healthy); default 2048, 0 = disable
	MaxDownloadSpeed string   `toml:"max_download_speed"` // e.g. "10MB", "500KB", "0" = unlimited
	MaxUploadSpeed   string   `toml:"max_upload_speed"`   // e.g. "1MB", "0" = unlimited
	// Seeding lifecycle (BitTorrent only). Off by default — the daemon leeches
	// then drops the torrent. Enable to keep uploading after a download finishes;
	// seeding stops at whichever target is hit first, or never if both are unset.
	SeedEnabled     bool    `toml:"seed_enabled"`      // keep uploading after completion (default: false)
	SeedRatio       float64 `toml:"seed_ratio"`        // stop once uploaded/size reaches this ratio (0 = no ratio target)
	SeedTime        string  `toml:"seed_time"`         // stop after this long since completion, e.g. "24h" (0/"" = no time target)
	MetadataTimeout string  `toml:"metadata_timeout"`  // e.g. "1h", "30m", "0" = unlimited (default: "0")
	StallTimeout    string  `toml:"stall_timeout"`     // e.g. "30m", "1h", "0" = unlimited (default: "30m")
	ListenPort      int     `toml:"listen_port"`       // fixed port for incoming peer connections (default: 42069, 0 = random)
	StreamPort      int     `toml:"stream_port"`       // fixed port for streaming HTTP server (default: 11818)
	HTTPSStreamPort int     `toml:"https_stream_port"` // HTTPS stream listener for direct valid-cert playback (default: 11819, 0 = disabled). Only serves once a certificate is present (agent-TLS feature).
	EnableUPnP      bool    `toml:"enable_upnp"`       // map StreamPort to the WAN via UPnP/NAT-PMP (default: false; opt-in)
	AutoHTTPSUpnp   bool    `toml:"auto_https_upnp"`   // auto-publish+renew the TLS+token HTTPS port via UPnP for remote direct-TLS (default: true; opt-out)
	// MaxStreamSessions caps simultaneous in-browser HLS stream sessions on this
	// daemon. Default 1 = the personal-agent model (one daemon == one viewer ==
	// one stream; a new session evicts the previous). Raise it on a SHARED /
	// server agent (e.g. the trial-remux VPS) so N viewers stream concurrently
	// instead of each new session killing the others. Only bounds the HLS path
	// (the multi-session registry); the single-file direct/remux-fMP4 path is
	// unaffected. 0/negative → treated as 1.
	//
	// INVARIANT (trial-remux VPS): keep this >= the web's trial concurrency cap
	// (TRIAL_ACCOUNT_CONCURRENCY in torrentclaw-web). If the web mints more
	// concurrent streamingSessions than this, the registry's LRU evicts a LIVE
	// viewer mid-stream. Note a re-open / quality-change / audio-switch creates
	// an extra session, so allow headroom above the raw viewer cap on a shared box.
	MaxStreamSessions int `toml:"max_stream_sessions"`
	// UsenetStreaming opts into on-the-fly Usenet streaming: when the web opens a
	// stream session for a Usenet (NZB) release, the daemon tries to serve it
	// straight from NNTP (arranca en segundos + seek) instead of downloading the
	// whole file first. Strictly OPT-IN and OFF by default — a streamable release
	// (plain .mkv/.mp4 or a STORE rar) plays instantly, while a non-streamable one
	// (compressed/encrypted RAR, password) falls back CLEANLY to the batch
	// download so playback is never blocked. Older daemons ignore the field.
	UsenetStreaming bool `toml:"usenet_streaming"`
	// RequireStreamToken gates remote (non-loopback) /stream + /hls requests on a
	// signed, short-lived token embedded in the URLs the agent reports. Default
	// true (secure by default); loopback callers (local mpv/vlc) are always exempt.
	// Set false only to debug a player that can't carry the token.
	RequireStreamToken bool     `toml:"require_stream_token"`
	CORSExtraOrigins   []string `toml:"cors_extra_origins"` // extra browser origins added on top of the baked-in allowlist (torrentclaw.com, app.torrentclaw.com, localhost:3030)
	// WebDAV read-only library export (opt-in, default false). When on, the stream
	// server also serves the ORGANIZED library over WebDAV at
	// http://<host>:<stream_port>/dav/ so Infuse / Kodi / VLC / Apple TV can browse
	// and play directly (no web player). READ-ONLY: only PROPFIND/GET/HEAD/OPTIONS —
	// every write verb (PUT/DELETE/MKCOL/COPY/MOVE) returns 405. Basic auth. Prefer
	// LAN / Tailscale, or the per-agent HTTPS listener, on untrusted networks.
	WebDAVEnabled bool `toml:"webdav_enabled"`
	// WebDAVUsername for Basic auth (default "unarr" when empty).
	WebDAVUsername string `toml:"webdav_username"`
	// WebDAVPassword for Basic auth. Empty = derive a STABLE password from the API
	// key (survives daemon restarts; shown by `unarr status`). Set to override.
	WebDAVPassword string `toml:"webdav_password"`
	// WebDAVAllowWAN lets /dav/ answer callers outside the local network
	// (loopback / RFC1918 / link-local / Tailscale CGNAT). Default false: the
	// stream port is reachable from the internet whenever it is published — by
	// auto_https_upnp on the TLS listener or enable_upnp on the cleartext one —
	// and /dav/ shares that mux, but its Basic auth has NO rate limiting, so an
	// exposed mount is an unthrottled password-guessing target. Remote playback
	// does not go through WebDAV, so leaving this off costs it nothing.
	WebDAVAllowWAN bool            `toml:"webdav_allow_wan"`
	Transcode      TranscodeConfig `toml:"transcode"`
	HLSCache       HLSCacheConfig  `toml:"hls_cache"`
	VPN            VPNConfig       `toml:"vpn"`
	Funnel         FunnelConfig    `toml:"funnel"`
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
	// Required turns the split-tunnel into a fail-CLOSED kill-switch for P2P.
	//
	//	true  — if the WireGuard tunnel is not up (fetch failed, slot held by
	//	        another device, config missing, or it dies mid-download) the
	//	        torrent/BitTorrent method is DISABLED so the user's IP is never
	//	        exposed to peers/trackers. Debrid + usenet (HTTPS/NNTP) keep
	//	        running — they don't leak the IP to a swarm.
	//	false — (default) current best-effort behavior: a failed tunnel logs a
	//	        warning and downloads continue in the clear.
	//
	// Zero value (false) preserves the exact pre-kill-switch behavior, so an
	// existing config that omits this key changes nothing.
	Required bool `toml:"required"`
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
	// Downlink selects the server→agent realtime transport. "auto" (default)
	// uses an SSE push connection with the long-poll wake as a buffering-tolerant
	// fallback; "sse" forces SSE only (no fallback); "poll" forces the pre-0.14
	// long-poll wake only. Empty = "auto". Available since unarr 0.14.0.
	Downlink string `toml:"downlink"`

	// LogMaxSizeMB is the size budget for unarr.log before it rotates to
	// unarr.log.1. DEFAULT 0 — rotation is OFF, and turning it on is OPT-IN.
	//
	// Off by default because rotating a file another process may be holding is
	// not a solved problem here, and shipping it on would hand every install the
	// unsolved part. Known limitations once it is on, all of them real and all
	// of them Windows-flavoured first: a second holder of the file (an open
	// `unarr logs -f`, an antivirus scanner, OneDrive/Dropbox syncing the data
	// dir, Windows Search) can block the rename or the truncate, and the
	// residual failure modes — a reader holding a rotated slot, two rotators
	// racing over one staging name, a live-but-offline daemon losing its
	// ownership claim — are listed under "Deuda abierta" in
	// docs/plans/daemon-log-ownership.md. Read that before setting this.
	//
	// With 0, unarr.log grows without bound: bound it with `unarr clean`, or
	// with an external logrotate using copytruncate (the daemon holds its
	// descriptor for the whole run and has no reopen signal).
	LogMaxSizeMB int `toml:"log_max_size_mb"`
	// LogMaxFiles is how many rotated copies to keep (unarr.log.1 … .N).
	// Default 3, so the whole ring costs at most (N+1) × log_max_size_mb.
	LogMaxFiles int `toml:"log_max_files"`
	// LogLevel is the minimum severity `unarr logs` shows by default:
	// "debug" | "info" | "warn" | "error". Default "info". The global
	// --log-level flag overrides it for a single invocation.
	LogLevel string `toml:"log_level"`
	// LogFormat is how `unarr logs` renders lines: "text" (exactly as written)
	// or "json" (one JSON object per line, for jq / a log shipper).
	// Default "text".
	LogFormat string `toml:"log_format"`
}

// Log-ring defaults.
//
// The slot count is NOT redefined here: internal/logging needs it as its own
// fallback (Options.keep, RotatedPaths resolve an unset MaxFiles with it), so
// that package owns the number and this one derives from it — two literals
// silently drifting apart is exactly the bug this avoids. internal/logging is
// stdlib-only, so the import adds no dependency weight and no cycle.
//
// The size budget has no counterpart over there on purpose: an unset MaxSizeMB
// already means "rotation disabled" in logging.Options, so this constant is a
// pure config policy number and this is its ONLY definition — every other place
// that needs it (Default(), applyDefaults, the boot log's own ring, the
// VBScript shim's trim) resolves it from the loaded config rather than
// re-literalising it.
//
// It is 0: rotation is opt-in. See DaemonConfig.LogMaxSizeMB for why, and
// docs/plans/daemon-log-ownership.md for what opting in costs.
const (
	defaultLogMaxSizeMB = 0
	defaultLogMaxFiles  = logging.DefaultMaxFiles
	defaultLogLevel     = "info"
	defaultLogFormat    = "text"
)

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

// TelemetryConfig gates the agent's lifecycle telemetry: the onboarding events
// (login_ok, first_sync, config/port/permission start-failures) and exit events
// (user_quit, crash, normal) the daemon reports so the server can see WHY an
// agent that registered never came back — instead of the current black box
// between register and the first download.
//
// Opt-out, off with a single switch. Telemetry is purely additive: disabling it
// must never degrade the product — the daemon still registers, syncs, and
// downloads exactly the same. When disabled the agent emits NOTHING (no event
// posts, and no exitReason on sync).
type TelemetryConfig struct {
	// Enabled defaults to true when unset. Pointer-vs-bool because Go's
	// zero-value bool would collapse "unset" (→ default on) and an explicit
	// `enabled = false` into the same false — same discipline as
	// DaemonConfig.AutoUpgrade. Override at runtime with UNARR_TELEMETRY=off
	// (env wins, applied by TelemetryEnabled — for headless/docker where editing
	// config.toml is awkward).
	Enabled *bool `toml:"enabled"`
}

// TelemetryEnabled resolves whether lifecycle telemetry should be emitted.
// Precedence (highest first):
//  1. UNARR_TELEMETRY env var — "off"/"0"/"false"/"no"/"disabled" (any case)
//     force it OFF; "on"/"1"/"true"/"yes" force it ON. Env wins so a headless
//     or docker deployment can opt out without editing config.toml.
//  2. [telemetry] enabled in config.toml.
//  3. Default: true (unset → on).
func (c *Config) TelemetryEnabled() bool {
	if v, ok := os.LookupEnv("UNARR_TELEMETRY"); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "off", "0", "false", "no", "disabled":
			return false
		case "on", "1", "true", "yes", "enabled":
			return true
		}
		// An unrecognised value falls through to the config/default below rather
		// than silently disabling — a typo must not turn telemetry off unnoticed.
	}
	if c.Telemetry.Enabled == nil {
		return true
	}
	return *c.Telemetry.Enabled
}

type LibraryConfig struct {
	ScanPath     string `toml:"scan_path"`     // remembered from last scan
	Workers      int    `toml:"workers"`       // concurrent ffprobe (default 8)
	FFprobePath  string `toml:"ffprobe_path"`  // optional explicit path
	FFmpegPath   string `toml:"ffmpeg_path"`   // optional explicit path (used by the HLS streaming transcoder)
	BackupDir    string `toml:"backup_dir"`    // for replaced files
	AutoScan     bool   `toml:"auto_scan"`     // enable daily auto-scan in daemon (default true)
	ScanInterval string `toml:"scan_interval"` // e.g. "1h", "6h", "24h" (default "1h", like Plex/Jellyfin periodic scans)
	AllowDelete  bool   `toml:"allow_delete"`  // allow web UI to request file deletion from disk

	// Sidecar caching: extract text subtitles (WebVTT) and thumbnail frames once
	// during the library scan and store them in a hidden ".unarr" dir next to the
	// media file, so the stream handlers serve them instantly instead of running
	// ffmpeg per request (and so huge remuxes don't hit the on-demand HTTP
	// timeout). Both default true; disable to save the disk/CPU of pre-extraction.
	CacheSubtitles  bool `toml:"cache_subtitles"`  // default true
	CacheThumbnails bool `toml:"cache_thumbnails"` // default true

	// CacheKeyframes builds the COPY-VOD keyframe index (a tiny .copyseg.json)
	// during the scan. Unlike the other sidecars this one cannot be rebuilt
	// cheaply at play time — it is a full container demux, measured at 153 s for
	// a 12 GB h264 over NFS — and without it a session falls back to EVENT copy,
	// which cannot honour a resume position. Needs only ffprobe, so it runs
	// independently of the ffmpeg-driven jobs above. Default true; disable to
	// spare the scan-time read at the cost of resume accuracy on cold files.
	CacheKeyframes bool `toml:"cache_keyframes"` // default true

	// Skip-segment detection: after each scan, find intro/credits ranges by
	// comparing chromaprint audio fingerprints between episodes of a season
	// (plus black-frame credits for movies) and submit them to the web so the
	// player can offer "Skip intro" / "Skip credits". Cached per file; only
	// new files do work. Default true.
	SkipDetect bool `toml:"skip_detect"`

	// Trickplay: at scan time, build ONE montage JPEG of frames sampled every
	// Interval seconds (+ a JSON manifest), cached in .unarr next to the media.
	// The web scrubber shows tiles from it — no live ffmpeg during playback, so
	// no contention with the active stream (the cause of broken seekbar previews)
	// — and the file panel picks a few positions from the same grid.
	Trickplay TrickplayConfig `toml:"trickplay"`

	// PrewarmMaxLoadRatio gates the heavy trickplay decode on system load: a sprite
	// job only starts while the 1-min load average is ≤ this × NumCPU, so scan-time
	// generation never saturates the machine or the NAS. Default 0.7; 0 falls back
	// to the default. Linux-only (no load reading elsewhere → unthrottled).
	PrewarmMaxLoadRatio float64 `toml:"prewarm_max_load_ratio"`

	// On-demand / automatic subtitle fetching from the web (Wyzie aggregator,
	// PRO). The web can always push a hot request (library/player button); this
	// section only controls SCAN-TIME auto-fetch, which is OFF by default.
	Subtitles SubtitlesConfig `toml:"subtitles"`

	// Cleanup ("library clean"): after each auto-scan the daemon reconciles the
	// download + library dirs, removing the deterministic junk that accretes there
	// (download stubs, orphaned partials/subtitles, byte-identical duplicate videos,
	// video-less dirs). Also runnable manually via `unarr library clean`.
	Cleanup CleanupConfig `toml:"cleanup"`
}

// CleanupConfig controls the library-hygiene sweep (`unarr library clean` and the
// daemon's automatic post-scan reconcile). Every category is individually
// toggleable; all default ON because each is deterministic and non-destructive to
// valid media (a real video >= MinVideoBytes is NEVER removed).
type CleanupConfig struct {
	// Enabled runs the sweep automatically after each auto-scan. The manual
	// `unarr library clean` command works regardless of this. Default true.
	Enabled bool `toml:"enabled"`

	// MinVideoBytes is the anti-stub floor: a video file smaller than this is a
	// download stub, not media. Human-readable size ("1MiB", "512KB", "2M"); empty
	// or unparseable falls back to 1 MiB (mirrors engine.minPlausibleVideoBytes).
	MinVideoBytes string `toml:"min_video_bytes"`

	// RemoveStubs removes video files below MinVideoBytes. Default true.
	RemoveStubs bool `toml:"remove_stubs"`

	// RemoveOrphanPartials removes .part/.!qB/.aria2/.tmp/.partial with no active
	// download task holding them. Default true.
	RemoveOrphanPartials bool `toml:"remove_orphan_partials"`

	// DedupExact collapses byte-identical copies of the same video within a dir
	// (verified by fingerprint: size + first/last 1 MiB), keeping one canonical
	// copy. Never touches files whose content differs (a real 2160p vs 1080p
	// upgrade coexists). Default true — the biggest space recovery.
	DedupExact bool `toml:"dedup_exact"`

	// RemoveOrphanSubtitles removes subtitles/sidecars (.srt/.nfo/.jpg/.par2/…)
	// with no owning video — checking the sidecar's own dir and, for per-track
	// ".unarr" cache sidecars, the parent release dir. Default true.
	RemoveOrphanSubtitles bool `toml:"remove_orphan_subtitles"`

	// PruneEmptyDirs removes directories that hold no valid video (empty or only
	// junk/stubs) and directories whose NAME is itself a media filename (a
	// mis-created "movie.mkv/" folder). Default true.
	PruneEmptyDirs bool `toml:"prune_empty_dirs"`

	// RemoveCorruptVideos removes videos of the right size (>= MinVideoBytes, so NOT
	// stubs) whose first and/or last 1 MiB is all NUL — "zero-content" downloads
	// that are unplayable yet pass the size floor and dodge the dedup. DEFAULT FALSE,
	// unlike every other category: the all-zero signal is a STRONG heuristic, not
	// proof, so the user opts in deliberately. Even enabled it is applied ONLY by the
	// manual `unarr library clean --apply` — the daemon's auto-sweep never removes a
	// corrupt video (KindCorruptVideo is intentionally not in reconcile's safeKinds).
	RemoveCorruptVideos bool `toml:"remove_corrupt_videos"`
}

// SubtitlesConfig controls scan-time subtitle auto-fetch.
type SubtitlesConfig struct {
	// AutoFetch: during a library scan, fetch missing subtitles for the preferred
	// languages and write them as sidecars. Default false (opt-in).
	AutoFetch bool `toml:"auto_fetch"`
	// Languages: preferred subtitle languages (ISO 639-1) to ensure exist, in
	// priority order, e.g. ["es", "en"]. Empty → auto-fetch does nothing.
	Languages []string `toml:"languages"`
}

// TrickplayConfig controls scan-time trickplay sprite generation.
type TrickplayConfig struct {
	Enabled  bool   `toml:"enabled"`  // generate the sprite during scan (default true)
	Interval string `toml:"interval"` // one frame per Interval, e.g. "10s" (default)
	Width    int    `toml:"width"`    // tile width px; height keeps aspect (default 240)
}

// IntervalSeconds parses Interval ("10s") to seconds, falling back to 10 on an
// empty/invalid value so a typo can't silently disable the sprite.
func (t TrickplayConfig) IntervalSeconds() float64 {
	if d, err := time.ParseDuration(strings.TrimSpace(t.Interval)); err == nil && d > 0 {
		return d.Seconds()
	}
	return 10
}

// validMethod reports whether s is a known download backend.
func validMethod(s string) bool {
	return s == "torrent" || s == "debrid" || s == "usenet"
}

// MethodOrder returns the effective ordered download-method preference, or nil
// for "auto" (defer to the web policy / torrent-first fallback). PreferredMethods
// (the list) wins; the legacy singular PreferredMethod is the fallback. "auto"
// anywhere collapses to nil. Unknown entries are dropped, dupes removed, order
// preserved. A nil/empty result means "no explicit preference".
func (c DownloadConfig) MethodOrder() []string {
	src := c.PreferredMethods
	if len(src) == 0 && c.PreferredMethod != "" {
		src = []string{c.PreferredMethod}
	}
	out := make([]string, 0, len(src))
	for _, m := range src {
		m = strings.ToLower(strings.TrimSpace(m))
		if m == "auto" {
			return nil // auto anywhere → defer
		}
		if validMethod(m) && !contains(out, m) {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// DefaultAPIURL is the API base a fresh install talks to. Exported so callers
// that need a fallback for an empty `auth.api_url` use THIS value instead of
// hardcoding their own: the managed-VPN call sites each hardcoded
// "https://torrentclaw.com", which silently aimed VPN fetches at a different
// deployment than the rest of the agent.
const DefaultAPIURL = "https://unarr.app"

// Default returns a Config with sensible defaults. Used both for fresh
// installs (no config file yet) and as the baseline for Load — fields not
// present in the user's TOML keep their Default() value.
func Default() Config {
	return Config{
		Auth: AuthConfig{
			APIURL: DefaultAPIURL,
			// Default mirror list (failover order). Primary is unarr.app — the
			// unarr brand's own deployment. Kept in sync with the brand-aware list
			// in src/lib/mirrors-config.ts; `unarr mirrors update` pulls the live
			// list from /api/v1/mirrors. torrentclaw.to is the non-CF (direct-to-
			// proxy) hop that survives a Cloudflare-anycast block (e.g. Spain
			// LaLiga); torrentclaw.com is a generic clearnet backup. All share the
			// same API + account table.
			Mirrors: []string{
				"https://torrentclaw.to",
				"https://torrentclaw.com",
			},
		},
		Download: DownloadConfig{
			PreferredMethod:    "auto",
			MaxConcurrent:      3,
			MinFreeDiskMB:      2048, // 2 GiB reserve
			StreamPort:         11818,
			HTTPSStreamPort:    11819,
			MaxStreamSessions:  1,    // personal-agent default: one stream at a time
			RequireStreamToken: true, // secure by default; loopback exempt
			AutoHTTPSUpnp:      true, // auto-publish the TLS+token HTTPS port for remote direct-TLS

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
			// Log ring: OFF (log_max_size_mb = 0), keeping 3 slots for whoever
			// turns it on. Written out explicitly, zero and all, so a fresh
			// config shows the knob exists instead of hiding it.
			LogMaxSizeMB: defaultLogMaxSizeMB,
			LogMaxFiles:  defaultLogMaxFiles,
			LogLevel:     defaultLogLevel,
			LogFormat:    defaultLogFormat,
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
			AutoScan:        true,
			ScanInterval:    "1h",
			Workers:         8,
			CacheSubtitles:  true,
			CacheThumbnails: true,
			CacheKeyframes:  true,
			SkipDetect:      true,
			Trickplay: TrickplayConfig{
				Enabled:  true,
				Interval: "10s",
				Width:    240,
			},
			PrewarmMaxLoadRatio: 0.7,
			Cleanup: CleanupConfig{
				Enabled:               true,
				MinVideoBytes:         "1MiB",
				RemoveStubs:           true,
				RemoveOrphanPartials:  true,
				DedupExact:            true,
				RemoveOrphanSubtitles: true,
				PruneEmptyDirs:        true,
				RemoveCorruptVideos:   false, // heuristic — opt-in only (see CleanupConfig)
			},
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

	// Keys the decoder ignored. Recorded, never fatal: failing here would break
	// every existing config the day a key gets renamed. Callers warn instead —
	// the daemon log, `unarr doctor`, `unarr config check`.
	for _, k := range meta.Undecoded() {
		cfg.unknownKeys = append(cfg.unknownKeys, k.String())
	}
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
		cfg.Auth.APIURL = DefaultAPIURL
	}
	if !meta.IsDefined("auth", "mirrors") {
		cfg.Auth.Mirrors = []string{"https://torrentclaw.to", "https://torrentclaw.com"}
	}
	if !meta.IsDefined("downloads", "preferred_method") {
		cfg.Download.PreferredMethod = "auto"
	}
	if !meta.IsDefined("downloads", "max_concurrent") {
		cfg.Download.MaxConcurrent = 3
	}
	if !meta.IsDefined("downloads", "min_free_disk_mb") {
		cfg.Download.MinFreeDiskMB = 2048 // 2 GiB reserve so a download never fills the FS to 0
	}
	if !meta.IsDefined("downloads", "stream_port") {
		cfg.Download.StreamPort = 11818
	}
	if !meta.IsDefined("downloads", "https_stream_port") {
		cfg.Download.HTTPSStreamPort = 11819
	}
	// Auto-publish the TLS+token HTTPS port to the WAN defaults ON for configs
	// predating the key, so remote browsers get the stable direct-TLS host instead
	// of the fragile CF funnel. Explicit `auto_https_upnp = false` opts out.
	if !meta.IsDefined("downloads", "auto_https_upnp") {
		cfg.Download.AutoHTTPSUpnp = true
	}
	if cfg.Download.MaxStreamSessions <= 0 {
		// Predates the key (or set to 0/negative) → personal-agent default of 1.
		// A shared/server agent sets it explicitly (e.g. 5 on the trial VPS).
		cfg.Download.MaxStreamSessions = 1
	}
	if !meta.IsDefined("general", "country") {
		cfg.General.Country = "US"
	}

	// Sidecar caching defaults ON for existing configs that predate these keys —
	// it only adds small hidden files next to media and makes subs/thumbnails
	// instant. Power users can set them false explicitly to opt out.
	if !meta.IsDefined("library", "cache_subtitles") {
		cfg.Library.CacheSubtitles = true
	}
	if !meta.IsDefined("library", "cache_thumbnails") {
		cfg.Library.CacheThumbnails = true
	}
	// Without this every pre-existing config would read as cache_keyframes=false
	// and the COPY-VOD index would stay unbuilt — the exact "prewarm silently
	// disabled" failure this key was added to fix.
	if !meta.IsDefined("library", "cache_keyframes") {
		cfg.Library.CacheKeyframes = true
	}
	if !meta.IsDefined("library", "skip_detect") {
		cfg.Library.SkipDetect = true
	}
	// Trickplay defaults ON for configs predating these keys (small sidecar JPEG;
	// makes the scrubber instant + contention-free). Explicit `enabled = false`
	// is respected via meta.IsDefined.
	if !meta.IsDefined("library", "trickplay", "enabled") {
		cfg.Library.Trickplay.Enabled = true
	}
	if !meta.IsDefined("library", "trickplay", "interval") {
		cfg.Library.Trickplay.Interval = "10s"
	}
	if !meta.IsDefined("library", "trickplay", "width") {
		cfg.Library.Trickplay.Width = 240
	}
	// Load-gate defaults ON for configs predating the key, so an old install can't
	// saturate the box with scan-time sprite generation.
	if !meta.IsDefined("library", "prewarm_max_load_ratio") {
		cfg.Library.PrewarmMaxLoadRatio = 0.7
	}

	// Log rotation defaults OFF for configs that predate the keys, and the
	// assignment stays rather than being dropped as a no-op: defaultLogMaxSizeMB
	// is the single definition of the policy, so flipping rotation back on for
	// new installs is a one-constant change and cannot forget this path. The
	// IsDefined guard (not a zero-check) is still what lets an explicit
	// `log_max_size_mb = 0` and an omitted key stay distinguishable here.
	if !meta.IsDefined("daemon", "log_max_size_mb") {
		cfg.Daemon.LogMaxSizeMB = defaultLogMaxSizeMB
	}
	if !meta.IsDefined("daemon", "log_max_files") {
		cfg.Daemon.LogMaxFiles = defaultLogMaxFiles
	}
	if !meta.IsDefined("daemon", "log_level") {
		cfg.Daemon.LogLevel = defaultLogLevel
	}
	if !meta.IsDefined("daemon", "log_format") {
		cfg.Daemon.LogFormat = defaultLogFormat
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

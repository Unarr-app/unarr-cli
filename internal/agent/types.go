package agent

import (
	"fmt"
	"time"
)

// RegisterRequest is sent by the CLI on startup to register itself.
type RegisterRequest struct {
	AgentID        string `json:"agentId"`
	Name           string `json:"name,omitempty"`
	OS             string `json:"os,omitempty"`
	Arch           string `json:"arch,omitempty"`
	Version        string `json:"version,omitempty"`
	DownloadDir    string `json:"downloadDir,omitempty"`
	DiskFreeBytes  int64  `json:"diskFreeBytes,omitempty"`
	DiskTotalBytes int64  `json:"diskTotalBytes,omitempty"`
	StreamPort     int    `json:"streamPort,omitempty"`
	LanIP          string `json:"lanIp,omitempty"`
	TailscaleIP    string `json:"tailscaleIp,omitempty"`
	// StreamSecret is the daemon's per-run HMAC key (hex) for stream tokens. The
	// web mints the HLS path token with it (the agent mints /stream tokens on its
	// own URLs); the agent verifies both. In memory, regenerated each start, so a
	// fresh register after restart re-syncs it.
	StreamSecret string `json:"streamSecret,omitempty"`
	// Transcode capabilities — let the web side suggest a smarter quality
	// before the player even starts. HWAccel is the picked backend
	// ("nvenc"/"qsv"/"vaapi"/"videotoolbox"/"none"). MaxTranscodeHeight is
	// the largest output resolution the agent can encode comfortably; for
	// software-only ffmpeg this is 1080p, with a real GPU encoder it goes
	// up to 2160p.
	HWAccel            string `json:"hwAccel,omitempty"`
	MaxTranscodeHeight int    `json:"maxTranscodeHeight,omitempty"`
	// Diagnostic surface filled by engine.DetectHWAccelDiagnostic at daemon
	// start. Surfaced in the web "Diagnose transcoder" modal so users can
	// see *why* their HWAccel landed on "none" without running
	// `unarr probe-hwaccel` locally — most commonly the ffmpeg binary
	// shipped without HW encoders (linuxbrew, brew's default formula).
	FFmpegVersion string   `json:"ffmpegVersion,omitempty"`
	FFmpegPath    string   `json:"ffmpegPath,omitempty"`
	HWEncoders    []string `json:"hwEncoders,omitempty"`
	HWDevices     []string `json:"hwDevices,omitempty"`
	// Managed-VPN split-tunnel state. The web tracks which agent holds the single
	// WireGuard slot (1 VPNResellers account = 1 WG keypair = 1 concurrent
	// connection); other agents are told to use OpenVPN on their host instead.
	// VPNActive has no omitempty: false is a meaningful state (tunnel down), not
	// "unset" — the server must see it to release the slot.
	VPNActive bool   `json:"vpnActive"`
	VPNMode   string `json:"vpnMode,omitempty"` // managed | self-hosted
	VPNServer string `json:"vpnServer,omitempty"`
	// CloudFlare Quick Tunnel hostname when enabled; the web prefers it over
	// Tailscale/LAN for in-browser playback because it works on any network.
	FunnelURL string `json:"funnelUrl,omitempty"`
}

// RegisterResponse is returned by the server after registration.
type RegisterResponse struct {
	Success  bool         `json:"success"`
	User     UserInfo     `json:"user"`
	Features FeatureFlags `json:"features"`
}

// UserInfo holds the authenticated user's profile.
type UserInfo struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Plan  string `json:"plan"`
	IsPro bool   `json:"isPro"`
}

// FeatureFlags indicates which download methods are available.
type FeatureFlags struct {
	Debrid       bool              `json:"debrid"`
	Usenet       bool              `json:"usenet"`
	UsenetServer *UsenetServerInfo `json:"usenetServer,omitempty"`
	Torrent      bool              `json:"torrent"`
}

// UsenetServerInfo holds NNTP connection details.
type UsenetServerInfo struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	SSL  bool   `json:"ssl"`
}

// Task represents a download task claimed from the server.
type Task struct {
	ID              string `json:"id"`
	InfoHash        string `json:"infoHash"`
	Title           string `json:"title"`
	ContentID       *int   `json:"contentId,omitempty"`
	IMDbID          string `json:"imdbId,omitempty"`
	PreferredMethod string `json:"preferredMethod"`          // auto | debrid | usenet | torrent
	Mode            string `json:"mode,omitempty"`           // download | stream
	DirectURL       string `json:"directUrl,omitempty"`      // HTTPS download URL (debrid, etc.)
	DirectFileName  string `json:"directFileName,omitempty"` // Original filename from direct URL
	NzbID           string `json:"nzbId,omitempty"`          // Pre-resolved NZB ID from server
	NzbPassword     string `json:"nzbPassword,omitempty"`    // Password for encrypted NZB archives
	ReplacePath     string `json:"replacePath,omitempty"`    // File to replace after download (upgrade mode)
	LibraryItemID   int    `json:"libraryItemId,omitempty"`  // Library item being upgraded
	ForceStart      bool   `json:"forceStart,omitempty"`     // Bypass queue (like Transmission's Force Start)
	ContentType     string `json:"contentType,omitempty"`    // "movie" | "show" — from server metadata
	ContentTitle    string `json:"contentTitle,omitempty"`   // Clean title from TMDB (e.g., "Frieren: Beyond Journey's End")
	Season          *int   `json:"season,omitempty"`         // Season number
	Episode         *int   `json:"episode,omitempty"`        // Episode number
	ContentYear     *int   `json:"contentYear,omitempty"`    // Year from TMDB (avoids regex on torrent title)
	CollectionName  string `json:"collectionName,omitempty"` // Collection name (e.g., "Harry Potter Collection")

	// FilePath is the on-disk path of the file the agent is being asked
	// to operate on. Currently used by mode=seed_file to know which
	// arbitrary file to wrap as a single-file torrent for browser
	// streaming; populated by the server from libraryItem.filePath.
	FilePath string `json:"filePath,omitempty"`
}

// StreamRequest is a request to stream a completed download from disk.
type StreamRequest struct {
	TaskID   string `json:"taskId"`
	FilePath string `json:"filePath"`
}

// StatusUpdate is sent by the CLI to report download progress.
type StatusUpdate struct {
	TaskID          string `json:"taskId"`
	Status          string `json:"status,omitempty"`   // downloading | completed | failed
	Progress        int    `json:"progress,omitempty"` // 0-100
	DownloadedBytes int64  `json:"downloadedBytes,omitempty"`
	TotalBytes      int64  `json:"totalBytes,omitempty"`
	SpeedBps        int64  `json:"speedBps,omitempty"`
	ETA             int    `json:"eta,omitempty"` // seconds remaining
	ResolvedMethod  string `json:"resolvedMethod,omitempty"`
	FileName        string `json:"fileName,omitempty"`
	FilePath        string `json:"filePath,omitempty"`
	StreamURL       string `json:"streamUrl,omitempty"`
	StreamReady     bool   `json:"streamReady,omitempty"`
	ErrorMessage    string `json:"errorMessage,omitempty"`
	// StreamError reports a failed /stream attempt (path rejected, transient
	// FS error, etc.) WITHOUT marking the download itself failed — the web
	// clears streamRequested + surfaces this so the player fails fast with the
	// real reason instead of a 20s "agent didn't respond" timeout.
	StreamError string `json:"streamError,omitempty"`
	// mode=seed_file: agent computes the info_hash from the local file
	// and reports it back so the web player can target /stream/<hash>.
	InfoHash string `json:"infoHash,omitempty"`
}

// StatusResponse is returned by the status endpoint.
// Includes flags the CLI must act on.
type StatusResponse struct {
	Success         bool `json:"success"`
	Cancelled       bool `json:"cancelled,omitempty"`
	Paused          bool `json:"paused,omitempty"`
	DeleteFiles     bool `json:"deleteFiles,omitempty"`
	StreamRequested bool `json:"streamRequested,omitempty"`
	Watching        bool `json:"watching,omitempty"`
}

// BatchStatusRequest wraps multiple status updates in a single request.
type BatchStatusRequest struct {
	Updates []StatusUpdate `json:"updates"`
}

// BatchStatusResponse wraps per-task results from the batch endpoint.
type BatchStatusResponse struct {
	Results  []StatusResponse `json:"results"`
	Watching bool             `json:"watching,omitempty"`
}

// UpgradeSignal tells the agent to upgrade to a specific version.
type UpgradeSignal struct {
	Version string `json:"version"`
}

// ErrorResponse is returned on API errors.
type ErrorResponse struct {
	Error   string `json:"error"`
	Details any    `json:"details,omitempty"`
}

// HTTPError represents an HTTP API error with a status code.
// Use errors.As to extract the status code for retry decisions.
type HTTPError struct {
	StatusCode int
	Message    string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("API error %d: %s", e.StatusCode, e.Message)
}

// AgentInfo holds metadata about the running agent for display.
type AgentInfo struct {
	ID          string
	Name        string
	User        UserInfo
	Features    FeatureFlags
	StartedAt   time.Time
	ActiveTasks int
}

// ---------------------------------------------------------------------------
// Usenet types
// ---------------------------------------------------------------------------

// UsenetCredentials holds NNTP connection details for the CLI.
type UsenetCredentials struct {
	Host           string `json:"host"`
	Port           int    `json:"port"`
	SSL            bool   `json:"ssl"`
	TLSServerName  string `json:"tlsServerName,omitempty"` // override for cert validation (e.g., "xsnews.nl")
	Username       string `json:"username"`
	Password       string `json:"password"`
	MaxConnections int    `json:"maxConnections"`
}

// NzbSearchParams defines search criteria for NZB indexers.
type NzbSearchParams struct {
	Query   string `json:"query,omitempty"`
	IMDbID  string `json:"imdbId,omitempty"`
	TVDbID  string `json:"tvdbId,omitempty"`
	Season  *int   `json:"season,omitempty"`
	Episode *int   `json:"episode,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

// NzbSearchResult represents a single NZB found by the indexer.
type NzbSearchResult struct {
	Title       string            `json:"title"`
	NzbID       string            `json:"nzbId"`
	Category    string            `json:"category"`
	Size        int64             `json:"size"`
	PublishedAt string            `json:"publishedAt"`
	Grabs       int               `json:"grabs"`
	Group       string            `json:"group"`
	Poster      string            `json:"poster"`
	Attributes  map[string]string `json:"attributes"`
}

// NzbSearchResponse wraps search results.
type NzbSearchResponse struct {
	Results []NzbSearchResult `json:"results"`
	Total   int               `json:"total"`
	Offset  int               `json:"offset"`
}

// UsenetUsageResponse holds quota information.
type UsenetUsageResponse struct {
	UsedBytes      int64   `json:"usedBytes"`
	QuotaBytes     int64   `json:"quotaBytes"`
	PercentUsed    float64 `json:"percentUsed"`
	RemainingBytes int64   `json:"remainingBytes"`
	QuotaResetDate string  `json:"quotaResetDate"`
}

// ---------------------------------------------------------------------------
// Batch download types (used by unarr migrate)
// ---------------------------------------------------------------------------

// BatchDownloadRequest sends a list of wanted items to queue for download.
type BatchDownloadRequest struct {
	Items         []WantedItem `json:"items"`
	ExcludeHashes []string     `json:"excludeHashes,omitempty"` // blocklisted + already-downloaded hashes
}

// WantedItem represents a movie or series the user wants.
type WantedItem struct {
	TmdbID int    `json:"tmdbId,omitempty"`
	ImdbID string `json:"imdbId,omitempty"`
	Title  string `json:"title"`
	Year   int    `json:"year,omitempty"`
	Type   string `json:"type"` // "movie" or "show"
}

// BatchDownloadResponse reports the outcome of a batch download request.
type BatchDownloadResponse struct {
	Queued        int         `json:"queued"`
	NotFound      int         `json:"notFound"`
	AlreadyActive int         `json:"alreadyActive"`
	Items         []BatchItem `json:"items"`
}

// BatchItem is the per-item result of a batch download.
type BatchItem struct {
	Title  string `json:"title"`
	Status string `json:"status"` // "queued", "not_found", "already_active"
}

// ---------------------------------------------------------------------------
// Debrid config types (used by unarr init/migrate)
// ---------------------------------------------------------------------------

// ConfigureDebridRequest configures a debrid provider.
type ConfigureDebridRequest struct {
	Provider string `json:"provider"` // "real-debrid", "alldebrid", "torbox", "premiumize"
	Token    string `json:"token"`
}

// ConfigureDebridResponse is returned after configuring a debrid provider.
type ConfigureDebridResponse struct {
	Success bool          `json:"success"`
	Account DebridAccount `json:"account"`
	Error   string        `json:"error,omitempty"`
}

// DebridAccount holds verified debrid account info.
type DebridAccount struct {
	Valid     bool   `json:"valid"`
	Premium   bool   `json:"premium"`
	Username  string `json:"username"`
	ExpiresAt string `json:"expiresAt,omitempty"`
}

// ---------------------------------------------------------------------------
// Library sync types (used by unarr scan)
// ---------------------------------------------------------------------------

// LibrarySyncRequest sends scanned media items to the server.
type LibrarySyncRequest struct {
	Items         []LibrarySyncItem `json:"items"`
	ScanPath      string            `json:"scanPath"`
	AgentID       string            `json:"agentId,omitempty"` // lets the server scope stale-cleanup per agent
	IsLastBatch   bool              `json:"isLastBatch"`
	SyncStartedAt string            `json:"syncStartedAt,omitempty"` // ISO-8601; same for all batches in a session
}

// LibrarySyncItem is a single scanned media file with ffprobe metadata.
type LibrarySyncItem struct {
	FilePath          string   `json:"filePath"`
	FileName          string   `json:"fileName"`
	FileSize          int64    `json:"fileSize,omitempty"`
	Title             string   `json:"title"`
	Year              string   `json:"year,omitempty"`
	Season            int      `json:"season,omitempty"`
	Episode           int      `json:"episode,omitempty"`
	ContentType       string   `json:"contentType"`
	Resolution        string   `json:"resolution,omitempty"`
	VideoCodec        string   `json:"videoCodec,omitempty"`
	HDR               string   `json:"hdr,omitempty"`
	BitDepth          int      `json:"bitDepth,omitempty"`
	AudioCodec        string   `json:"audioCodec,omitempty"`
	AudioChannels     int      `json:"audioChannels,omitempty"`
	AudioLanguages    []string `json:"audioLanguages,omitempty"`
	SubtitleLanguages []string `json:"subtitleLanguages,omitempty"`
	AudioTracks       any      `json:"audioTracks,omitempty"`
	SubtitleTracks    any      `json:"subtitleTracks,omitempty"`
	VideoInfo         any      `json:"videoInfo,omitempty"`
	// Integrity flags a damaged / incompletely-downloaded file ("damaged" or
	// empty). IntegrityReason is a stable code (ebml_corrupt, moov_missing,
	// no_duration, …) the web maps to a localized "re-download" message.
	Integrity       string `json:"integrity,omitempty"`
	IntegrityReason string `json:"integrityReason,omitempty"`
	// Path resilience: a stable content identity + the file's location relative
	// to its library root, so the server can move a row in place on a rename /
	// base-path change instead of duplicating it.
	Fingerprint    string `json:"fingerprint,omitempty"`
	RelPath        string `json:"relPath,omitempty"`
	LibraryRootKey string `json:"libraryRootKey,omitempty"`
}

// LibrarySyncResponse is returned after syncing library items.
type LibrarySyncResponse struct {
	Synced  int `json:"synced"`
	Matched int `json:"matched"`
	Removed int `json:"removed"`
}

// ---------------------------------------------------------------------------
// Sync types (unified CLI ↔ Server communication)
// ---------------------------------------------------------------------------

// SyncRequest is sent by the CLI periodically to synchronize state with the server.
// Contains the CLI's full execution state — the server responds with pending actions.
type SyncRequest struct {
	AgentID         string      `json:"agentId"`
	Version         string      `json:"version,omitempty"`
	OS              string      `json:"os,omitempty"`
	Arch            string      `json:"arch,omitempty"`
	Name            string      `json:"name,omitempty"`
	DownloadDir     string      `json:"downloadDir,omitempty"`
	DiskFreeBytes   int64       `json:"diskFreeBytes,omitempty"`
	DiskTotalBytes  int64       `json:"diskTotalBytes,omitempty"`
	StreamPort      int         `json:"streamPort,omitempty"`
	LanIP           string      `json:"lanIp,omitempty"`
	TailscaleIP     string      `json:"tailscaleIp,omitempty"`
	FreeSlots       int         `json:"freeSlots"`
	Tasks           []TaskState `json:"tasks"`
	CanDelete       bool        `json:"canDelete"`                 // library.allow_delete is enabled
	DeleteConfirmed []int       `json:"deleteConfirmed,omitempty"` // library item IDs successfully deleted from disk
	// Live managed-VPN split-tunnel state, sent every sync so the web sees the
	// WireGuard slot owner update in near-realtime (vs. register, once at startup).
	// VPNActive has no omitempty: false (tunnel down) must reach the server so it
	// releases the slot, not be elided as "unset".
	VPNActive bool   `json:"vpnActive"`
	VPNMode   string `json:"vpnMode,omitempty"`
	VPNServer string `json:"vpnServer,omitempty"`
	// CloudFlare Quick Tunnel hostname when enabled, else empty.
	FunnelURL string `json:"funnelUrl,omitempty"`
}

// ControlAction represents a server-side control signal for a task.
type ControlAction struct {
	Action      string `json:"action"` // "pause", "resume", "cancel", "stream"
	TaskID      string `json:"taskId"`
	DeleteFiles bool   `json:"deleteFiles,omitempty"`
}

// LibraryDeleteRequest is a server-side request to delete a file from disk.
type LibraryDeleteRequest struct {
	ItemID   int    `json:"itemId"`
	FilePath string `json:"filePath"`
}

// StreamSession is a request to open an HLS streaming session for an
// in-browser player. The CLI registers the HLS session in the StreamServer's
// HLS registry; source bytes come from FilePath (or, when only InfoHash is
// set, from a download_task on disk).
type StreamSession struct {
	SessionID string `json:"sessionId"`
	FilePath  string `json:"filePath,omitempty"`
	InfoHash  string `json:"infoHash,omitempty"`
	TaskID    string `json:"taskId,omitempty"`
	FileName  string `json:"fileName,omitempty"`
	FileSize  int64  `json:"fileSize,omitempty"`
	// Quality target the daemon should aim for when transcoding. One of
	// "2160p" | "1080p" | "720p" | "480p" | "original" | "" (defer to config).
	Quality string `json:"quality,omitempty"`
	// AudioIndex selects the source audio track (-map 0:a:N). -1 means
	// "use the default/first track".
	AudioIndex int `json:"audioIndex,omitempty"`
	// BurnSubtitleIndex, when set, is the 0-based subtitle stream index
	// (-map 0:s:N) of a BITMAP subtitle (PGS/DVB) to burn into the video. Text
	// subtitles are served as separate WebVTT tracks and never burned. A pointer
	// (not int) so absent/null = "no burn": the zero value 0 is a valid track
	// index, so an int sentinel would silently burn track 0 when the field is
	// omitted. Forces a full video re-encode (the overlay can't ride a copy
	// path), so the web only sends it when the user picks a bitmap sub.
	BurnSubtitleIndex *int `json:"burnSubtitleIndex,omitempty"`
	// PlayMethod is how the daemon should serve this session:
	//   ""       — default (HLS transcode); also what legacy servers send.
	//   "direct" — the source is already browser-native (the web decided this
	//              from library scan metadata + an agent-version gate). Serve
	//              the raw file over /stream (HTTP Range, no ffmpeg) instead of
	//              transcoding to HLS. See hueco #3 phase 3a in the roadmap.
	PlayMethod string `json:"playMethod,omitempty"`
	// DirectURL, when set, is an HTTPS link to the media resolved server-side
	// from the user's debrid account (hueco #2 / 2a). The source has no local
	// file: the daemon streams /stream from this URL via ranged GETs
	// (debridFileProvider) instead of from disk/torrent. Carries the "play
	// instantáneo cache-fast" promise — the web only sets it when the hash is
	// confirmed debrid-cached and the container is browser-native (mp4/m4v),
	// and gates it on an agent-version floor so older daemons never receive a
	// field they can't serve. Takes priority over FilePath when present.
	DirectURL string `json:"directUrl,omitempty"`
}

// SyncResponse is returned by the server with all pending actions for the CLI.
type SyncResponse struct {
	NewTasks       []Task                 `json:"newTasks,omitempty"`
	Controls       []ControlAction        `json:"controls,omitempty"`
	StreamRequests []StreamRequest        `json:"streamRequests,omitempty"`
	StreamSessions []StreamSession        `json:"streamSessions,omitempty"`
	Watching       bool                   `json:"watching"`
	Upgrade        *UpgradeSignal         `json:"upgrade,omitempty"`
	Scan           bool                   `json:"scan,omitempty"`
	FilesToDelete  []LibraryDeleteRequest `json:"filesToDelete,omitempty"`
}

// ---------------------------------------------------------------------------
// Watch progress types (used by stream tracking)
// ---------------------------------------------------------------------------

// WatchProgressUpdate reports playback position during streaming.
// Two modes:
//   - Estimated (range): set Progress (0-100). Position/Duration omitted.
//   - Precise (browser): set Position + Duration in seconds. Progress computed server-side.
type WatchProgressUpdate struct {
	TaskID   string `json:"taskId"`
	Source   string `json:"source"`             // "range" or "browser"
	Progress *int   `json:"progress,omitempty"` // 0-100 (range source)
	Position *int   `json:"position,omitempty"` // seconds (browser source)
	Duration *int   `json:"duration,omitempty"` // seconds (browser source)
}

// WatchProgressResponse is returned after reporting watch progress.
type WatchProgressResponse struct {
	Success bool `json:"success"`
}

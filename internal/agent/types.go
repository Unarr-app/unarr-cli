package agent

import "time"

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

// HeartbeatRequest is sent every 30s to keep the agent alive.
type HeartbeatRequest struct {
	AgentID        string `json:"agentId"`
	Name           string `json:"name,omitempty"`
	Version        string `json:"version,omitempty"`
	OS             string `json:"os,omitempty"`
	DownloadDir    string `json:"downloadDir,omitempty"`
	DiskFreeBytes  int64  `json:"diskFreeBytes,omitempty"`
	DiskTotalBytes int64  `json:"diskTotalBytes,omitempty"`
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
}

// TasksResponse wraps the array of tasks returned by the server.
type TasksResponse struct {
	Tasks          []Task          `json:"tasks"`
	StreamRequests []StreamRequest `json:"streamRequests,omitempty"`
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
	ErrorMessage    string `json:"errorMessage,omitempty"`
}

// StatusResponse is returned by the status endpoint.
// Includes flags the CLI must act on.
type StatusResponse struct {
	Success         bool `json:"success"`
	Cancelled       bool `json:"cancelled,omitempty"`
	Paused          bool `json:"paused,omitempty"`
	DeleteFiles     bool `json:"deleteFiles,omitempty"`
	StreamRequested bool `json:"streamRequested,omitempty"`
}

// BatchStatusRequest wraps multiple status updates in a single request.
type BatchStatusRequest struct {
	Updates []StatusUpdate `json:"updates"`
}

// BatchStatusResponse wraps per-task results from the batch endpoint.
type BatchStatusResponse struct {
	Results []StatusResponse `json:"results"`
}

// HeartbeatResponse is returned by the server on heartbeat.
type HeartbeatResponse struct {
	Success  bool           `json:"success"`
	Upgrade  *UpgradeSignal `json:"upgrade,omitempty"`
	Watching bool           `json:"watching,omitempty"` // true when a user is viewing download progress in the web UI
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

// AgentInfo holds metadata about the running agent for display.
type AgentInfo struct {
	ID          string
	Name        string
	User        UserInfo
	Features    FeatureFlags
	StartedAt   time.Time
	LastPollAt  time.Time
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
	Items       []LibrarySyncItem `json:"items"`
	ScanPath    string            `json:"scanPath"`
	IsLastBatch bool              `json:"isLastBatch"`
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
}

// LibrarySyncResponse is returned after syncing library items.
type LibrarySyncResponse struct {
	Synced  int `json:"synced"`
	Matched int `json:"matched"`
	Removed int `json:"removed"`
}

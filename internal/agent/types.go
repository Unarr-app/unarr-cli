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
	PreferredMethod string `json:"preferredMethod"` // auto | debrid | usenet | torrent
	Mode            string `json:"mode,omitempty"`   // download | stream
	DirectURL       string `json:"directUrl,omitempty"`      // HTTPS download URL (debrid, etc.)
	DirectFileName  string `json:"directFileName,omitempty"` // Original filename from direct URL
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
	Status          string `json:"status,omitempty"`          // downloading | completed | failed
	Progress        int    `json:"progress,omitempty"`        // 0-100
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

// HeartbeatResponse is returned by the server on heartbeat.
type HeartbeatResponse struct {
	Success bool           `json:"success"`
	Upgrade *UpgradeSignal `json:"upgrade,omitempty"`
}

// UpgradeSignal tells the agent to upgrade to a specific version.
type UpgradeSignal struct {
	Version string `json:"version"`
}

// UpgradeResult is sent by the agent after an upgrade attempt.
type UpgradeResult struct {
	AgentID string `json:"agentId"`
	Success bool   `json:"success"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
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

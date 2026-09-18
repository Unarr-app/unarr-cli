// Package diagnostics prepares privacy-preserving, explicitly requested reports.
// Raw logs, configuration, command output and errors never cross this schema.
package diagnostics

type Report struct {
	SchemaVersion int         `json:"schemaVersion"`
	AgentID       string      `json:"agentId"`
	GeneratedAt   string      `json:"generatedAt"`
	System        System      `json:"system"`
	Service       Service     `json:"service"`
	Settings      Settings    `json:"settings"`
	Logs          []LogSource `json:"logs"`
}

type System struct {
	OS               string `json:"os"`
	Arch             string `json:"arch"`
	OSVersion        string `json:"osVersion"`
	AppVersion       string `json:"appVersion"`
	GoVersion        string `json:"goVersion"`
	CPUs             int    `json:"cpus"`
	MemoryMiB        uint64 `json:"memoryMiB"`
	DownloadFreeMiB  uint64 `json:"downloadFreeMiB"`
	DownloadTotalMiB uint64 `json:"downloadTotalMiB"`
}

type Service struct {
	Manager      string `json:"manager"`
	State        string `json:"state"`
	LastExitCode *int   `json:"lastExitCode,omitempty"`
	DaemonState  string `json:"daemonState"`
	ActiveTasks  int    `json:"activeTasks"`
}

type Settings struct {
	VPNEnabled    bool `json:"vpnEnabled"`
	VPNRequired   bool `json:"vpnRequired"`
	FunnelEnabled bool `json:"funnelEnabled"`
	WebDAVEnabled bool `json:"webdavEnabled"`
	AutoUpgrade   bool `json:"autoUpgrade"`
}

type LogSource struct {
	Source       string  `json:"source"`
	Status       string  `json:"status"`
	ScannedBytes int     `json:"scannedBytes"`
	OmittedLines int     `json:"omittedLines"`
	Events       []Event `json:"events"`
}

type Event struct {
	Event string `json:"event"`
	Time  string `json:"time,omitempty"`
	Code  *int   `json:"code,omitempty"`
}

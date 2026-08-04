// Package control is the daemon's local control plane: a loopback HTTP server
// the daemon exposes, and the client the CLI and the desktop tray use to drive
// it.
//
// Why a local plane at all, when the web can already cancel and pause: because
// every web control rides on the task row. When the row is gone — the user
// cancelled and then removed the entry — the web has no way left to reach the
// download, while the agent happily keeps it in its resume store and restarts
// it on every boot. That is a download nobody can stop (user report 2026-08-03).
// The local plane is the answer to "how do I force this to stop", and it works
// with the server unreachable, the row deleted, or the account logged out.
//
// It is deliberately NOT part of the stream server: that one is reachable from
// the LAN (and, with the funnel, from the internet). Control binds to loopback
// only and requires a token that lives in the daemon state file, readable by
// the owning user alone.
package control

// Action names accepted by the control plane. Kept as plain strings (not an
// enum type) because they cross an HTTP boundary and appear verbatim in CLI
// arguments, log lines, and the tray menu.
const (
	ActionPause  = "pause"
	ActionResume = "resume"
	ActionCancel = "cancel"
	ActionRetry  = "retry"
	ActionPurge  = "purge"
)

// TaskInfo is one row of `unarr downloads` — the union of what the manager is
// running right now and what the resume store still holds. A task that is
// persisted but not running is either paused or a leftover awaiting
// reconciliation, and the difference matters to the user, so State says which.
type TaskInfo struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// State is the manager's status ("downloading", "verifying", …) or one of
	// the store-only states "paused" / "stopped".
	State           string `json:"state"`
	Progress        int    `json:"progress"`
	DownloadedBytes int64  `json:"downloadedBytes,omitempty"`
	TotalBytes      int64  `json:"totalBytes,omitempty"`
	SpeedBps        int64  `json:"speedBps,omitempty"`
	ETA             int    `json:"eta,omitempty"`
	Method          string `json:"method,omitempty"`
	FileName        string `json:"fileName,omitempty"`
	ErrorMessage    string `json:"errorMessage,omitempty"`
	// Running distinguishes a task the manager is actually working on from one
	// that only exists in the resume store.
	Running bool `json:"running"`
	// Persisted reports whether the resume store holds a payload for this task,
	// i.e. whether a daemon restart would bring it back.
	Persisted bool `json:"persisted"`
}

// ActionRequest is the body of POST /tasks/{action}.
type ActionRequest struct {
	// TaskID is the full task id, or a unique short prefix (the ids the CLI and
	// the logs print are 8-char prefixes, and asking a user to retype a UUID to
	// stop a runaway download is hostile).
	TaskID string `json:"taskId,omitempty"`
	// All applies the action to every task the plane knows about.
	All bool `json:"all,omitempty"`
	// DeleteFiles asks a cancel to remove partial files from disk too.
	DeleteFiles bool `json:"deleteFiles,omitempty"`
}

// ActionResult reports what happened to one task.
type ActionResult struct {
	TaskID string `json:"taskId"`
	Title  string `json:"title,omitempty"`
	// Applied is false when the task was found but the action was a no-op
	// (resuming something already running, say).
	Applied bool   `json:"applied"`
	Message string `json:"message,omitempty"`
}

// ActionResponse is the body of a control action response.
type ActionResponse struct {
	Results []ActionResult `json:"results"`
	// Error is set (with a non-200 status) when the request itself was bad —
	// unknown task, ambiguous prefix, unsupported action.
	Error string `json:"error,omitempty"`
}

// ListResponse is the body of GET /tasks.
type ListResponse struct {
	Tasks []TaskInfo `json:"tasks"`
}

// Controller is what the daemon plugs into the server: the actions the plane
// can perform. Implemented in package cmd, where the manager, the resume store
// and the agent client all live.
type Controller interface {
	List() []TaskInfo
	Pause(taskID string) ActionResult
	Resume(taskID string) ActionResult
	Cancel(taskID string, deleteFiles bool) ActionResult
	Retry(taskID string) ActionResult
	// Purge drops resume-store entries for tasks that are NOT running — the
	// zombie cleanup. Returns one result per dropped entry.
	Purge() []ActionResult
}

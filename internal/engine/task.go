package engine

import (
	"fmt"
	"sync"
	"time"

	"github.com/torrentclaw/unarr/internal/agent"
)

// TaskStatus represents the current state of a download task.
type TaskStatus string

const (
	StatusPending     TaskStatus = "pending"
	StatusClaimed     TaskStatus = "claimed"
	StatusResolving   TaskStatus = "resolving"
	StatusDownloading TaskStatus = "downloading"
	StatusVerifying   TaskStatus = "verifying"
	StatusOrganizing  TaskStatus = "organizing"
	StatusSeeding     TaskStatus = "seeding"
	StatusCompleted   TaskStatus = "completed"
	StatusFailed      TaskStatus = "failed"
	StatusCancelled   TaskStatus = "cancelled"
)

// validTransitions defines allowed state changes.
var validTransitions = map[TaskStatus][]TaskStatus{
	StatusPending:     {StatusClaimed},
	StatusClaimed:     {StatusResolving, StatusCancelled},
	StatusResolving:   {StatusDownloading, StatusFailed, StatusCancelled},
	StatusDownloading: {StatusVerifying, StatusFailed, StatusResolving, StatusCancelled},
	StatusVerifying:   {StatusOrganizing, StatusFailed},
	StatusOrganizing:  {StatusSeeding, StatusCompleted},
	StatusSeeding:     {StatusCompleted},
}

// Task represents a download task with its full lifecycle state.
type Task struct {
	mu sync.RWMutex

	// From server
	ID              string
	InfoHash        string
	Title           string
	ContentID       *int
	IMDbID          string
	PreferredMethod string // auto | torrent | debrid | usenet
	DirectURL       string // HTTPS download URL (debrid, etc.)
	DirectFileName  string // Original filename from direct URL
	NzbID           string // Pre-resolved NZB ID (usenet)
	NzbPassword     string // Password for encrypted NZB archives
	ReplacePath     string // File to replace after download (upgrade mode)
	LibraryItemID   int    // Library item being upgraded

	// Runtime state
	Status          TaskStatus
	Mode            string // download | stream
	ResolvedMethod  DownloadMethod
	TriedMethods    []DownloadMethod
	DownloadedBytes int64
	TotalBytes      int64
	SpeedBps        int64
	ETA             int
	FileName        string
	FilePath        string
	StreamURL       string
	ErrorMessage    string

	// Timestamps
	ClaimedAt   time.Time
	StartedAt   time.Time
	CompletedAt time.Time
}

// NewTaskFromAgent creates a Task from a server-claimed agent.Task.
func NewTaskFromAgent(at agent.Task) *Task {
	mode := at.Mode
	if mode == "" {
		mode = "download"
	}
	return &Task{
		ID:              at.ID,
		InfoHash:        at.InfoHash,
		Title:           at.Title,
		ContentID:       at.ContentID,
		IMDbID:          at.IMDbID,
		PreferredMethod: at.PreferredMethod,
		DirectURL:       at.DirectURL,
		DirectFileName:  at.DirectFileName,
		NzbID:           at.NzbID,
		NzbPassword:     at.NzbPassword,
		ReplacePath:     at.ReplacePath,
		LibraryItemID:   at.LibraryItemID,
		Mode:            mode,
		Status:          StatusClaimed,
		ClaimedAt:       time.Now(),
	}
}

// Transition validates and performs a state transition.
func (t *Task) Transition(to TaskStatus) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	allowed, ok := validTransitions[t.Status]
	if !ok {
		return fmt.Errorf("no transitions from %s", t.Status)
	}
	for _, a := range allowed {
		if a == to {
			t.Status = to
			if to == StatusDownloading {
				t.StartedAt = time.Now()
			}
			if to == StatusCompleted || to == StatusFailed {
				t.CompletedAt = time.Now()
			}
			return nil
		}
	}
	return fmt.Errorf("invalid transition: %s -> %s", t.Status, to)
}

// GetStatus returns current status thread-safely.
func (t *Task) GetStatus() TaskStatus {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.Status
}

// SetStreamURL sets the stream URL thread-safely.
func (t *Task) SetStreamURL(url string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.StreamURL = url
}

// GetStreamURL returns the stream URL thread-safely.
func (t *Task) GetStreamURL() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.StreamURL
}

// UpdateProgress updates download metrics thread-safely.
func (t *Task) UpdateProgress(p Progress) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.DownloadedBytes = p.DownloadedBytes
	t.TotalBytes = p.TotalBytes
	t.SpeedBps = p.SpeedBps
	t.ETA = p.ETA
	if p.FileName != "" {
		t.FileName = p.FileName
	}
}

// Percent returns download progress as 0-100.
func (t *Task) Percent() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.TotalBytes <= 0 {
		return 0
	}
	p := int(float64(t.DownloadedBytes) / float64(t.TotalBytes) * 100)
	if p > 100 {
		return 100
	}
	return p
}

// ToStatusUpdate converts task state to an API status update.
func (t *Task) ToStatusUpdate() agent.StatusUpdate {
	t.mu.RLock()
	defer t.mu.RUnlock()

	apiStatus := ""
	switch t.Status {
	case StatusResolving:
		apiStatus = "resolving"
	case StatusDownloading:
		apiStatus = "downloading"
	case StatusVerifying:
		apiStatus = "verifying"
	case StatusOrganizing:
		apiStatus = "organizing"
	case StatusSeeding:
		apiStatus = "downloading"
	case StatusCompleted:
		apiStatus = "completed"
	case StatusFailed:
		apiStatus = "failed"
	default:
		// StatusPending, StatusClaimed, StatusCancelled — not reported
	}

	return agent.StatusUpdate{
		TaskID:          t.ID,
		Status:          apiStatus,
		Progress:        t.Percent(),
		DownloadedBytes: t.DownloadedBytes,
		TotalBytes:      t.TotalBytes,
		SpeedBps:        t.SpeedBps,
		ETA:             t.ETA,
		ResolvedMethod:  string(t.ResolvedMethod),
		FileName:        t.FileName,
		FilePath:        t.FilePath,
		StreamURL:       t.StreamURL,
		ErrorMessage:    t.ErrorMessage,
	}
}

// MagnetURI builds a magnet link from the info hash.
func (t *Task) MagnetURI() string {
	return "magnet:?xt=urn:btih:" + t.InfoHash
}

// HasUntried returns true if there are download methods not yet attempted.
func (t *Task) HasUntried(available []DownloadMethod) bool {
	for _, m := range available {
		tried := false
		for _, tm := range t.TriedMethods {
			if tm == m {
				tried = true
				break
			}
		}
		if !tried {
			return true
		}
	}
	return false
}

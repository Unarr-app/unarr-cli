package engine

import (
	"fmt"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Unarr-app/unarr-cli/internal/agent"
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
	// Verifying → Resolving: the on-disk verify found a truncated/corrupt file and
	// the manager is re-downloading the same source (bounded integrity retry).
	StatusVerifying:  {StatusOrganizing, StatusFailed, StatusResolving},
	StatusOrganizing: {StatusSeeding, StatusCompleted},
	StatusSeeding:    {StatusCompleted},
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
	ContentType     string // "movie" | "show" — from server metadata
	ContentTitle    string // Clean title from TMDB
	Season          *int   // Season number
	Episode         *int   // Episode number
	ContentYear     *int   // Year from TMDB (avoids regex on torrent title)
	CollectionName  string // Collection name (e.g., "Harry Potter Collection")

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

	// onChange, when set, is called after every successful status Transition so
	// the daemon can push the new state to the server immediately (event-driven
	// uplink) instead of waiting for the next sync tick. Must be non-blocking —
	// it's a coalescing TriggerSync. Set by the Manager at submit time.
	onChange func()
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
		ContentType:     at.ContentType,
		ContentTitle:    at.ContentTitle,
		ContentYear:     at.ContentYear,
		Season:          at.Season,
		Episode:         at.Episode,
		CollectionName:  at.CollectionName,
		Mode:            mode,
		Status:          StatusClaimed,
		ClaimedAt:       time.Now(),
	}
}

// Transition validates and performs a state transition. On success it invokes
// the onChange hook (outside the lock) so the daemon can push the new state to
// the server immediately rather than waiting for the next sync tick.
func (t *Task) Transition(to TaskStatus) error {
	t.mu.Lock()

	allowed, ok := validTransitions[t.Status]
	if !ok {
		t.mu.Unlock()
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
			cb := t.onChange
			t.mu.Unlock()
			// Fire the event-driven uplink AFTER releasing the lock so a future
			// heavier hook can't deadlock on the task mutex.
			if cb != nil {
				cb()
			}
			return nil
		}
	}
	t.mu.Unlock()
	return fmt.Errorf("invalid transition: %s -> %s", t.Status, to)
}

// SetOnChange wires the post-transition hook. Call before the task starts
// transitioning (the Manager sets it at submit time).
func (t *Task) SetOnChange(fn func()) {
	t.mu.Lock()
	t.onChange = fn
	t.mu.Unlock()
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

// SetResolvedMethod records the resolved download method thread-safely. The
// download goroutine writes it (resolve/fallback) while API-handler goroutines
// (cancel/pause) and the progress reporter (ToStatusUpdate) read it — so every
// access must go through the task mutex. Do NOT read it directly inside a
// section that already holds t.mu (e.g. ToStatusUpdate): RWMutex is not
// reentrant.
func (t *Task) SetResolvedMethod(m DownloadMethod) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ResolvedMethod = m
}

// GetResolvedMethod returns the resolved download method thread-safely.
func (t *Task) GetResolvedMethod() DownloadMethod {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.ResolvedMethod
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

	// Compute percent inline — do NOT call t.Percent() here since we already hold RLock.
	// Calling Percent() (which also RLocks) while holding RLock deadlocks when a writer is waiting.
	percent := 0
	if t.TotalBytes > 0 {
		percent = int(float64(t.DownloadedBytes) / float64(t.TotalBytes) * 100)
		if percent > 100 {
			percent = 100
		}
	}

	return agent.StatusUpdate{
		TaskID:          t.ID,
		Status:          apiStatus,
		Progress:        percent,
		DownloadedBytes: t.DownloadedBytes,
		TotalBytes:      t.TotalBytes,
		SpeedBps:        t.SpeedBps,
		ETA:             t.ETA,
		ResolvedMethod:  string(t.ResolvedMethod),
		FileName:        t.FileName,
		FilePath:        t.FilePath,
		StreamURL:       t.StreamURL,
		// Cap to the server's stored length. A failed extract can carry a
		// multi-KB unrar/par2 dump; sending it raw made /agent/status 400
		// the whole report, leaving the task stuck non-terminal.
		ErrorMessage: truncateMsg(t.ErrorMessage, 2000),
	}
}

// truncateMsg caps s to at most max bytes without splitting a UTF-8 rune.
func truncateMsg(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
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

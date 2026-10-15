package engine

import (
	"net/http"
	"time"
)

// HLSInfo is a read-only snapshot of one HLS session for the daemon's session
// view (internal/metrics). It copies values out under the session's own locks,
// so the caller never holds one.
type HLSInfo struct {
	ID        string
	FileName  string
	Prewarm   bool
	VideoCopy bool
	StartedAt time.Time
	LastTouch time.Time
	// Failed: the auto-restart supervisor gave up; the session is dead.
	Failed bool
	// ServedBytes is what the HTTP handler has written to viewers.
	ServedBytes int64
	Stats       TranscodeStats
}

// Info snapshots the session.
func (s *HLSSession) Info() HLSInfo {
	s.mu.Lock()
	info := HLSInfo{
		ID:        s.cfg.SessionID,
		FileName:  s.cfg.FileName,
		Prewarm:   s.cfg.Prewarm,
		VideoCopy: s.cfg.VideoCopy,
		StartedAt: s.startedAt,
		LastTouch: s.lastTouch,
	}
	s.mu.Unlock()
	info.Failed, _ = s.Failed()
	info.ServedBytes = s.servedBytes.Load()
	info.Stats = s.GetTranscodeStats()
	return info
}

// Infos snapshots every registered session.
func (r *HLSSessionRegistry) Infos() []HLSInfo {
	r.mu.RLock()
	sessions := make([]*HLSSession, 0, len(r.sessions))
	for _, s := range r.sessions {
		sessions = append(sessions, s)
	}
	r.mu.RUnlock()
	out := make([]HLSInfo, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, s.Info())
	}
	return out
}

// servedWriter counts the bytes a handler writes to a viewer. It forwards
// Flush and exposes Unwrap so http.ResponseController (deadlines, hijack) still
// reaches the real writer.
type servedWriter struct {
	http.ResponseWriter
	add func(int64)
}

func (w *servedWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if n > 0 {
		w.add(int64(n))
	}
	return n, err
}

func (w *servedWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *servedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// countServed wraps w so every byte written is added to the session's counter.
func (s *HLSSession) countServed(w http.ResponseWriter) http.ResponseWriter {
	return &servedWriter{ResponseWriter: w, add: func(n int64) { s.servedBytes.Add(n) }}
}

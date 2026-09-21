package engine

import (
	"encoding/json"
	"net/http"
)

// subtitleSidecarsComplete reports whether every WebVTT sidecar this session
// serves under /hls/<id>/subs/ has been fully written.
//
//   - COPY-VOD remote: the whole-file extractor exited (subsDone closed).
//   - EVENT copy: the sidecars are extra outputs of the single remux pass, so
//     they are final once that ffmpeg exited.
//   - no extractor at all (no text tracks): vacuously complete.
func (s *HLSSession) subtitleSidecarsComplete() bool {
	if s.subsDone != nil {
		select {
		case <-s.subsDone:
			return true
		default:
			return false
		}
	}
	if s.copyVOD {
		return true
	}
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	return s.exited
}

// ServeSubtitleStatus answers GET /hls/<id>/subs/status.json.
//
// A browser fetches a <track> exactly once, and the sidecar is served one-shot
// even while it is still being written (holding the response open instead stalls
// playback — see ServeSubtitleVTT). So a client that fetched early holds a
// partial track. This tells it when re-fetching will finally yield the complete
// one. Until then `progress`, when present, grows each time more cues became
// available (they arrive in viewer-first order, not playback order, so the
// client cannot infer it from the cues it holds); without it the client may
// re-fetch as the playhead outruns the cues it has.
func (s *HLSSession) ServeSubtitleStatus(w http.ResponseWriter, _ *http.Request) {
	s.Touch()
	status := struct {
		Complete bool `json:"complete"`
		Progress *int `json:"progress,omitempty"`
	}{Complete: s.subtitleSidecarsComplete()}
	if s.subWin != nil {
		n := s.subWin.progress()
		status.Progress = &n
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(status)
}

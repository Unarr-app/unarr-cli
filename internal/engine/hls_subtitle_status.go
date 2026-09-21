package engine

import (
	"encoding/json"
	"net/http"
)

// subtitleSidecarsComplete reports whether every WebVTT sidecar this session
// serves under /hls/<id>/subs/ has been fully written.
//
//   - COPY-VOD remote: every segment's window was extracted (subsDone closed),
//     which only happens once the whole file was watched.
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
// available (they follow the viewer, not playback order, so the client cannot
// infer it from the cues it holds) and `covered` lists the [start, end] ranges,
// in seconds, already extracted — a client may tell the viewer that subtitles
// are still loading while the playhead sits outside them. COPY-VOD only ever
// extracts what is watched, so `complete` may stay false for a whole session.
// Without `progress` the client may re-fetch as the playhead outruns its cues.
//
// Deliberately no s.Touch(): this is polled for as long as the page is open, and
// `complete` may never turn true, so counting it as activity kept a paused,
// forgotten tab's session — ffmpeg, source proxy, debrid link — alive forever.
// Watching is what keeps a session alive: segment and sidecar requests.
func (s *HLSSession) ServeSubtitleStatus(w http.ResponseWriter, _ *http.Request) {
	status := struct {
		Complete bool         `json:"complete"`
		Progress *int         `json:"progress,omitempty"`
		Covered  [][2]float64 `json:"covered"` // null: not a windowed session
	}{Complete: s.subtitleSidecarsComplete()}
	if s.subWin != nil {
		n := s.subWin.progress()
		status.Progress = &n
		status.Covered = s.subWin.covered()
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(status)
}

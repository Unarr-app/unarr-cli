package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func subtitleStatus(t *testing.T, s *HLSSession) bool {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeSubtitleStatus(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status: code=%d cache=%q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	var body struct {
		Complete *bool `json:"complete"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Complete == nil {
		t.Fatalf("status body %q: %v", rec.Body.String(), err)
	}
	return *body.Complete
}

// The web player re-fetches a partial subtitle track once this flips to true, so
// it must be false exactly while a sidecar can still grow.
func TestSubtitleStatusTracksExtraction(t *testing.T) {
	// COPY-VOD remote: follows the whole-file extractor.
	s := &HLSSession{copyVOD: true, subsDone: make(chan struct{})}
	if subtitleStatus(t, s) {
		t.Fatal("complete while the extractor is still running")
	}
	close(s.subsDone)
	if !subtitleStatus(t, s) {
		t.Fatal("incomplete after the extractor exited")
	}

	// COPY-VOD with no text tracks: no extractor, nothing will ever grow.
	if !subtitleStatus(t, &HLSSession{copyVOD: true}) {
		t.Fatal("session without an extractor must report complete")
	}

	// EVENT copy: sidecars are outputs of the remux pass → final when it exits.
	ev := &HLSSession{}
	if subtitleStatus(t, ev) {
		t.Fatal("EVENT copy complete while ffmpeg is still running")
	}
	ev.readyMu.Lock()
	ev.exited = true
	ev.readyMu.Unlock()
	if !subtitleStatus(t, ev) {
		t.Fatal("EVENT copy incomplete after ffmpeg exited")
	}
}

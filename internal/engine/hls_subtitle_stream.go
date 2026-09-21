package engine

import (
	"bytes"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/library/mediainfo"
)

const (
	subtitleStreamPoll = 500 * time.Millisecond
	// A comment block every so often keeps idle-timeout proxies (Cloudflare
	// tunnel: 100 s) from cutting a response whose extractor is being held back.
	subtitleStreamKeepalive = 20 * time.Second
	// Backstop only: the stream normally ends when the extractor exits.
	subtitleStreamMax = 45 * time.Minute
)

// subtitleExtractionRunning reports whether this session's whole-file subtitle
// extractor is still writing sidecars.
func (s *HLSSession) subtitleExtractionRunning() bool {
	if s.subsDone == nil {
		return false
	}
	select {
	case <-s.subsDone:
		return false
	default:
		return true
	}
}

// streamSubtitleVTT serves a sidecar that is STILL BEING WRITTEN as one
// long-lived response: cues are sent as the extractor produces them and the
// response ends when extraction does.
//
// Why not just serve what is on disk: a browser fetches a <track> exactly once.
// Handing it the file mid-extraction froze the track at the cues read so far —
// subtitles worked from the start, then vanished for good after a seek past that
// point (field report: gone at minute 15, while switching to another track, which
// was fetched after extraction finished, worked). Browsers add cues from a track
// response incrementally while it loads (verified in Chromium: cues grow with
// readyState still LOADING), so a streamed response gives both: subtitles
// immediately, and a complete track.
func (s *HLSSession) streamSubtitleVTT(w http.ResponseWriter, r *http.Request, path string) {
	h := w.Header()
	h.Set("Content-Type", "text/vtt; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform") // no-transform: keep CDNs from buffering to compress
	h.Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)

	sent := 0 // raw sidecar bytes already delivered; always on a block boundary
	lastWrite := time.Now()
	deadline := time.NewTimer(subtitleStreamMax)
	defer deadline.Stop()
	tick := time.NewTicker(subtitleStreamPoll)
	defer tick.Stop()
	for {
		// Sample "done" BEFORE reading so the final pass sees the whole file.
		final := !s.subtitleExtractionRunning()
		chunk, next := nextSubtitleChunk(path, sent, final)
		if len(chunk) == 0 && !final && time.Since(lastWrite) > subtitleStreamKeepalive {
			chunk = []byte("NOTE keepalive\n\n")
		}
		if len(chunk) > 0 {
			if !s.sendSubtitleChunk(w, rc, chunk) {
				return
			}
			lastWrite = time.Now()
		}
		sent = next
		if final {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			return
		case <-tick.C:
		}
	}
}

// sendSubtitleChunk writes and flushes one chunk; false means stop streaming.
func (s *HLSSession) sendSubtitleChunk(w http.ResponseWriter, rc *http.ResponseController, chunk []byte) bool {
	if _, err := w.Write(chunk); err != nil {
		return false // client went away
	}
	if err := rc.Flush(); err != nil {
		log.Printf("[hls %s] subtitle stream cannot flush: %v", shortHLSID(s.cfg.SessionID), err)
		return false
	}
	return true
}

// nextSubtitleChunk returns the filtered cues in sidecar bytes [sent, cut) and
// the new offset. Mid-extraction cut is the end of the last COMPLETE block, so a
// half-written cue is never sent; on the final pass it is the end of the file.
// The drawing-cue filter is block-local, so filtering chunk by chunk yields the
// same cues as filtering the finished file.
func nextSubtitleChunk(path string, sent int, final bool) (chunk []byte, next int) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is tmpDir + a caller-validated non-negative index.
	if err != nil || len(raw) <= sent {
		return nil, sent
	}
	cut := len(raw)
	if !final {
		i := bytes.LastIndex(raw[sent:], []byte("\n\n"))
		if i < 0 {
			return nil, sent
		}
		cut = sent + i + 2
	}
	return mediainfo.FilterVTTDrawingCues(raw[sent:cut]), cut
}

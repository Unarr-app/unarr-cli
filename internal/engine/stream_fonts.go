package engine

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/library/mediainfo"
)

// fontsHandler serves ONE font attachment muxed into a media container:
//
//	GET /fonts?p=<media path>&i=<attachment index>&n=<filename>&t=<token>
//
// Fansub .ass tracks name fonts the viewer will not have installed. Without
// them libass falls back to a default face and the typesetting — sign sizes,
// line breaks, glyph coverage for non-Latin scripts — comes out wrong. The
// fonts travel inside the same MKV as the subtitle, so the agent is the only
// component that can hand them to the player.
//
// `i` is ffmpeg's `t:N` attachment ordinal (see mediainfo.FontAttachment.Index),
// NOT a global stream index. `n` is the original filename, used only to pick the
// file extension and Content-Type; the bytes served are decided by `i` alone, so
// a tampered `n` cannot select a different attachment.
func (ss *StreamServer) fontsHandler(w http.ResponseWriter, r *http.Request) {
	ss.lastActivity.Store(time.Now().UnixNano())
	if ss.writeCORSHeaders(w, r, "") {
		return
	}

	q := r.URL.Query()
	rawPath := q.Get("p")
	if rawPath == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	index, err := strconv.Atoi(q.Get("i"))
	if err != nil || index < 0 {
		http.Error(w, "bad index", http.StatusBadRequest)
		return
	}
	// One token covers every font of a file (see streamScopeFonts) — an .ass
	// track routinely needs a dozen, and per-attachment tokens would bloat the
	// session payload without narrowing what a leaked URL exposes.
	if !ss.checkStreamToken(streamScopeFonts(rawPath), q.Get("t")) {
		clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)
		log.Printf("[fonts] rejected from %s - bad/absent token", clientIP)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	filename := q.Get("n")

	// A URL source (debrid/HLS-from-URL) has no local file. Dumping an
	// attachment means ffmpeg parsing the container header over the network,
	// which for a remote 20GB MKV is slow and pointless — the subtitle renderer
	// degrades to its fallback font instead.
	if strings.Contains(rawPath, "://") {
		http.Error(w, "fonts unavailable for remote sources", http.StatusNotFound)
		return
	}

	rawPath = ss.healMediaPath(rawPath) // host→container base-path skew (see /thumbnail)
	if fi, statErr := os.Stat(rawPath); statErr != nil || !fi.Mode().IsRegular() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	if font, ok := mediainfo.ReadCachedFont(rawPath, index, filename); ok {
		ss.writeFont(w, font, filename)
		return
	}

	if ss.ffmpegPath == "" {
		http.Error(w, "fonts unavailable", http.StatusServiceUnavailable)
		return
	}

	ss.extractAndServeFont(w, r, rawPath, filename, index)
}

// extractAndServeFont runs the cold-cache path of fontsHandler: bounded ffmpeg
// attachment dump, best-effort cache write, response.
func (ss *StreamServer) extractAndServeFont(w http.ResponseWriter, r *http.Request, rawPath, filename string, index int) {
	// Attachments live in the container header, so the dump reads the first few
	// MB and stops — far cheaper than a subtitle extract, which has to demux the
	// whole runtime. 30s is generous even on slow network storage.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Bound concurrent dumps: unlike /thumbnail (human-paced), a libass player
	// asks for EVERY font of a file at once — the corpus MKV names 26 — and each
	// cold-cache request would otherwise spawn its own ffmpeg parsing the same
	// container header. Three at a time keeps a cold start snappy without
	// letting a burst (or several clients) stack dozens of processes on a slow
	// NAS. Cache hits never reach this.
	select {
	case fontExtractSem <- struct{}{}:
		defer func() { <-fontExtractSem }()
	case <-ctx.Done():
		log.Printf("[fonts] extract queue timeout (i=%d path=%q)", index, rawPath)
		http.Error(w, "font extract busy", http.StatusServiceUnavailable)
		return
	}

	font, err := mediainfo.ExtractFontAttachment(ctx, ss.ffmpegPath, rawPath, index, filename)
	if err != nil {
		log.Printf("[fonts] extract failed (i=%d path=%q): %v", index, rawPath, err)
		http.Error(w, "font extract failed", http.StatusInternalServerError)
		return
	}
	if ss.cacheSubtitles {
		if werr := mediainfo.WriteCachedFont(rawPath, index, filename, font); werr != nil {
			log.Printf("[fonts] cache write skipped (i=%d path=%q): %v", index, rawPath, werr)
		}
	}
	ss.writeFont(w, font, filename)
}

// fontExtractSem bounds concurrent ffmpeg attachment dumps (see fontsHandler).
var fontExtractSem = make(chan struct{}, 3)

// writeFont writes a font attachment response. Fonts inside a container are
// immutable for that file, so they cache for a day; private because the media
// is the user's.
func (ss *StreamServer) writeFont(w http.ResponseWriter, font []byte, filename string) {
	w.Header().Set("Content-Type", mediainfo.FontContentType(filename))
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("Content-Length", strconv.Itoa(len(font)))
	//nolint:gosec // G705: binary font data served as font/*, never as HTML. The
	// bytes come from a token-scoped, stat'd local container via ffmpeg's
	// attachment dump; the browser hands them to libass, not to a parser that
	// could execute them.
	if _, err := w.Write(font); err != nil {
		log.Printf("[fonts] write failed: %v", err)
	}
}

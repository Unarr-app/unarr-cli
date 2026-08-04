// Package engine — stream_usenet.go exposes the /usenet/<id> HTTP endpoint that
// range-serves an on-the-fly Usenet stream so ffmpeg can consume it as a
// SourceURL (-i http://127.0.0.1:<port>/usenet/<id>?t=<token>). It is the local
// HTTP costura the container-remux / HLS path needs: hls.go drives ffmpeg with a
// URL, never an io.Reader, so the NNTP-range reader must be reachable as a URL.
//
// Unlike /stream (one file at a time, swapped via SetFile), /usenet is a KEYED
// registry: the daemon registers a source under an opaque id, hands ffmpeg the
// tokenised loopback URL, and unregisters when the session ends. Several sources
// can be live at once (e.g. a remux reading one release while a direct-play
// serves another) without displacing each other.
//
// Auth mirrors /stream: an HMAC stream token, here scoped usenet:<id> so a token
// minted for one source never validates another. This endpoint's only legitimate
// consumer is local ffmpeg, so an absent/invalid token returns a plain 401 (not
// the no-oracle 404 that publicly-reachable /stream uses) — clearer for a local
// caller, and /usenet is never advertised on the funnel.
package engine

import (
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// usenetSourceRegistry maps an opaque id to the FileProvider that serves it.
// Guarded by an RWMutex: registration/unregistration happen on the daemon's
// session goroutines while the handler reads concurrently for every ranged GET.
type usenetSourceRegistry struct {
	mu      sync.RWMutex
	sources map[string]FileProvider
}

func newUsenetSourceRegistry() *usenetSourceRegistry {
	return &usenetSourceRegistry{sources: make(map[string]FileProvider)}
}

// register stores (or replaces) the provider for id. A nil provider is ignored
// with a log line rather than stored — a registered nil would 500 the handler on
// the first read, and the plan returns nil for a non-streamable release, so this
// keeps a botched wiring from poisoning the endpoint.
func (reg *usenetSourceRegistry) register(id string, provider FileProvider) {
	if provider == nil {
		log.Printf("[usenet-stream] refusing to register nil provider for id %q", id)
		return
	}
	reg.mu.Lock()
	reg.sources[id] = provider
	reg.mu.Unlock()
}

// unregister drops id from the registry. Safe to call for an unknown id (no-op).
func (reg *usenetSourceRegistry) unregister(id string) {
	reg.mu.Lock()
	delete(reg.sources, id)
	reg.mu.Unlock()
}

// get returns the provider for id, or nil when none is registered.
func (reg *usenetSourceRegistry) get(id string) FileProvider {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	return reg.sources[id]
}

// count returns how many sources are currently registered (diagnostics/tests).
func (reg *usenetSourceRegistry) count() int {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	return len(reg.sources)
}

// RegisterUsenetSource publishes a Usenet stream provider under id so it becomes
// reachable at /usenet/<id>. The daemon calls this before handing ffmpeg (or a
// player) the URL from UsenetLoopbackURL. Replacing an existing id is allowed
// (last-writer-wins), mirroring SetFile's swap semantics.
func (ss *StreamServer) RegisterUsenetSource(id string, provider FileProvider) {
	ss.usenet.register(id, provider)
	log.Printf("[usenet-stream] registered source %q (%d active)", id, ss.usenet.count())
}

// UnregisterUsenetSource removes a previously-registered source. Call when the
// session ends so a stale id can't be re-fetched. No-op for an unknown id.
func (ss *StreamServer) UnregisterUsenetSource(id string) {
	ss.usenet.unregister(id)
	log.Printf("[usenet-stream] unregistered source %q (%d active)", id, ss.usenet.count())
}

// ActiveUsenetSources returns the number of registered Usenet sources.
func (ss *StreamServer) ActiveUsenetSources() int { return ss.usenet.count() }

// UsenetLoopbackURL returns the tokenised loopback URL ffmpeg consumes for a
// registered source: http://127.0.0.1:<port>/usenet/<id>?t=<token>. Loopback
// because the consumer is the local ffmpeg process — the stream never leaves the
// host. The token is scoped usenet:<id> so it authorises only this one source.
// Returns "" for an empty id.
func (ss *StreamServer) UsenetLoopbackURL(id string) string {
	if id == "" {
		return ""
	}
	base := "http://127.0.0.1:" + strconv.Itoa(ss.port) + "/usenet/" + id
	if !ss.requireToken {
		return base
	}
	return base + "?t=" + mintStreamToken(ss.streamSecret, streamScopeUsenet(id), time.Now())
}

// usenetHandler range-serves the FileProvider registered under the id in the
// request path (/usenet/<id>). The flow mirrors StreamServer.handler for the
// raw-provider case, with a keyed lookup instead of the single current file:
// CORS preflight → token check (scoped to the id) → registry lookup → open a
// fresh reader → http.ServeContent (which Seeks for size + range start, then
// Reads). On any error it fails cleanly (no partial/corrupt body).
func (ss *StreamServer) usenetHandler(w http.ResponseWriter, r *http.Request) {
	ss.lastActivity.Store(time.Now().UnixNano())

	clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	log.Printf("[usenet-stream] %s /usenet from %s Range:%q", r.Method, clientIP, r.Header.Get("Range"))

	if ss.writeCORSHeaders(w, r, "Content-Length, Content-Range, Accept-Ranges") {
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/usenet/")
	// The id is a single path segment. Reject a missing or multi-segment / bad
	// id up front — same accepted alphabet as HLS session ids, so a traversal or
	// injection attempt never reaches the registry lookup.
	if id == "" || !validSessionID.MatchString(id) {
		http.Error(w, "bad usenet id", http.StatusNotFound)
		return
	}

	// Auth: the token must be a valid signature for THIS id's scope. An absent or
	// wrong token is a 401 — /usenet is a local ffmpeg endpoint, so a clear
	// Unauthorized beats the no-oracle 404 /stream needs for its public reach.
	if !ss.checkStreamToken(streamScopeUsenet(id), r.URL.Query().Get("t")) {
		log.Printf("[usenet-stream] rejected %s for id %q - bad/absent token", clientIP, id)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	provider := ss.usenet.get(id)
	if provider == nil {
		http.Error(w, "no such usenet source", http.StatusNotFound)
		return
	}

	reader := provider.NewFileReader(r.Context())
	if reader == nil {
		http.Error(w, "usenet source unavailable", http.StatusNotFound)
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", mimeTypeFromExt(provider.FileName()))
	w.Header().Set("Accept-Ranges", "bytes")
	// no-store: the source is streamed on the fly and the tokenised URL is
	// single-session — nothing here should be cached by an intermediary.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", "inline")

	// http.ServeContent handles HEAD (headers + size, no body), Range (206 +
	// Content-Range), and full GET (200) uniformly, driving the reader's
	// Seek/Read exactly as the debrid/disk providers expect.
	http.ServeContent(w, r, provider.FileName(), time.Time{}, reader)
}

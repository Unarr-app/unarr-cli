package engine

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/torrentclaw/unarr/internal/library/mediainfo"
)

// StreamURLs holds all available stream URLs keyed by network type.
// Serialized as JSON into the stream_url DB field so the web API can
// pick the best URL based on the browser's IP address.
type StreamURLs struct {
	LAN       string `json:"lan,omitempty"`
	Tailscale string `json:"ts,omitempty"`
	Public    string `json:"pub,omitempty"`
}

// FileProvider abstracts where to get a file reader for streaming.
type FileProvider interface {
	NewFileReader(ctx context.Context) io.ReadSeekCloser
	FileName() string
	FileSize() int64
}

// GrowingSource is a /stream source whose bytes are produced over time by an
// ffmpeg remux/transcode to a temp file (see transcodeSource). It is served
// via manual Range handling (serveGrowing) instead of http.ServeContent,
// which assumes a complete, fixed-size, seekable file. Used by direct-play's
// remux path (hueco #3 / 3b): mkv h264/aac → progressive fMP4, no re-encode.
type GrowingSource interface {
	// ReadAt blocks until off+len(p) bytes have been produced, the source is
	// final, or a timeout elapses; near the live edge it returns a short
	// (n>0, nil) read so the caller can stream what exists so far.
	ReadAt(p []byte, off int64) (int, error)
	Size() int64          // bytes produced so far
	Final() bool          // ffmpeg exited — Size() is now the true total
	EstimatedSize() int64 // expected final size, for the scrubber timeline
	FileName() string
	Close() error
}

// StreamServer is a persistent HTTP server that serves one file at a time.
// Start it once with Listen(), then swap files with SetFile()/ClearFile().
// The server stays alive for the entire daemon lifecycle — no port churn.
type StreamServer struct {
	mu       sync.RWMutex
	provider FileProvider
	growing  GrowingSource // set instead of provider for the progressive-remux path (3b)
	taskID   string        // current task being streamed

	server      *http.Server
	port        int
	url         string     // best single URL (backward compat)
	urls        StreamURLs // all available URLs by network type
	upnpMapping *UPnPMapping

	// TLS — optional HTTPS listener for direct, valid-cert browser playback
	// (agent-TLS feature). httpsPort 0 = disabled. tlsCert holds the current
	// server certificate, swapped atomically on renewal; the TLS config reads it
	// via GetCertificate so a renewed cert applies without dropping the listener.
	// HTTP (port) keeps serving regardless — loopback players + the funnel use it.
	httpsPort   int
	httpsServer *http.Server
	tlsCert     atomic.Pointer[tls.Certificate]
	// enableUPnP gates whether Listen() asks the gateway to publish the
	// stream port to the WAN. UPnP is opt-in (false by default) because
	// /stream and /hls have no auth — exposing them on the public internet
	// would let any scanner enumerate active downloads. LAN and Tailscale
	// access keep working without UPnP.
	enableUPnP bool
	// corsExtraOrigins are operator-configured origins added to the default
	// allowlist defined in validate.go. Set before Listen().
	corsExtraOrigins []string
	// corsAllowlist is computed at Listen() time and treated as read-only
	// thereafter so per-request reads need no locking.
	corsAllowlist map[string]struct{}

	hls *HLSSessionRegistry // HLS sessions served on /hls/<id>/...

	// streamSecret signs the per-URL stream tokens (see stream_token.go). In
	// memory only; regenerated each daemon start. requireToken gates whether
	// remote (non-loopback) /stream and /hls requests must carry a valid token.
	streamSecret []byte
	requireToken bool

	// ffmpegPath is the resolved ffmpeg binary, used by /thumbnail to extract a
	// single frame on demand. Empty = thumbnails disabled (503). Set once before
	// Listen() via SetFFmpegPath; read-only thereafter so the handler needs no lock.
	ffmpegPath string

	// cacheSubtitles / cacheThumbnails enable write-through caching of extracted
	// WebVTT / JPEG frames into the hidden ".unarr" sidecar dir next to the media
	// (mirrors the scan-time prewarm). Set once before Listen() via the setters;
	// default false here, flipped on from config (default true) by the daemon.
	cacheSubtitles  bool
	cacheThumbnails bool

	// trickplayWidth is the tile width (px) the scan-time prewarm used to build
	// the trickplay sprite (library.trickplay.width). The /trickplay handler keys
	// the sidecar lookup on it so the agent owns the width — the web need not know
	// it. 0 = trickplay disabled (the handler 404s and the web falls back to
	// on-demand /thumbnail). Set once before Listen() via SetTrickplayWidth.
	trickplayWidth int

	lastActivity    atomic.Int64
	maxByteOffset   atomic.Int64 // highest sequential read position (main playback connection)
	totalFileSize   atomic.Int64
	bitrateBps      atomic.Int64 // video bitrate in bits/sec (from ffprobe, 0 = unknown)
	durationSec     atomic.Int64 // video duration in seconds (from ffprobe, 0 = unknown)
	topReaderID     atomic.Int64 // ID of the reader that set maxByteOffset (only it can advance it)
	readerCounter   atomic.Int64 // monotonic counter for assigning reader IDs
	speedtestActive atomic.Bool  // single-flight guard for /speedtest (unauth + public via funnel)
}

// NewStreamServer creates a stream server bound to the given port.
// Call Listen() to start accepting connections, then SetFile() to serve content.
//
// UPnP is opt-in: call SetUPnPEnabled(true) before Listen() to publish the
// stream port on the WAN. Without it, only LAN and Tailscale clients can
// reach the server. This matches the security default — /stream and /hls
// have no auth, so exposing them to the public internet is something the
// operator must explicitly request.
func NewStreamServer(port int) *StreamServer {
	return &StreamServer{
		port:         port,
		hls:          NewHLSSessionRegistry(),
		streamSecret: newStreamSecret(),
		requireToken: true, // secure by default; the agent self-mints tokens
	}
}

// StreamSecretHex returns the daemon's stream-token signing key as hex, so it
// can be reported to the web (which mints the HLS path token the agent then
// verifies). Treat as a secret — it lets the holder mint valid stream tokens.
func (ss *StreamServer) StreamSecretHex() string {
	return hex.EncodeToString(ss.streamSecret)
}

// SetRequireStreamToken toggles remote stream-token enforcement. Loopback
// callers are always exempt. Call before Listen() / before reporting URLs.
// Default is true; an operator can disable it via config for debugging.
func (ss *StreamServer) SetRequireStreamToken(require bool) {
	ss.requireToken = require
}

// checkStreamToken reports whether a request may proceed: always true when
// enforcement is off; otherwise the token must be a valid signature for scope.
// No loopback exemption — cloudflared relays public funnel traffic over
// localhost, so loopback is not a trust signal.
func (ss *StreamServer) checkStreamToken(scope, token string) bool {
	if !ss.requireToken {
		return true
	}
	return verifyStreamToken(ss.streamSecret, scope, token, time.Now())
}

// SetUPnPEnabled toggles WAN publishing of the stream port. Call before
// Listen(); changes after Listen() are ignored for the active server.
func (ss *StreamServer) SetUPnPEnabled(enabled bool) {
	ss.enableUPnP = enabled
}

// EnableTLS arms the HTTPS listener on httpsPort. Call before Listen(). The
// listener starts even without a certificate installed yet — handshakes fail
// until one is set via SetTLSCertificate, so a cert issued asynchronously (the
// future ACME broker) applies live without a restart. httpsPort <= 0 is a no-op.
func (ss *StreamServer) EnableTLS(httpsPort int) {
	if httpsPort > 0 {
		ss.httpsPort = httpsPort
	}
}

// SetTLSCertificate atomically installs or replaces the server certificate used
// by the HTTPS listener. Safe to call at any time (startup or on renewal); the
// new cert applies to the next TLS handshake without dropping the listener.
func (ss *StreamServer) SetTLSCertificate(cert *tls.Certificate) {
	ss.tlsCert.Store(cert)
}

// LoadTLSCertificateFromFiles reads a PEM cert+key pair from disk and installs
// it. Returns an error if the pair is missing or invalid — the caller decides
// whether that's fatal (the daemon treats it as "TLS off, HTTP keeps serving").
func (ss *StreamServer) LoadTLSCertificateFromFiles(certPath, keyPath string) error {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return fmt.Errorf("load TLS keypair: %w", err)
	}
	ss.SetTLSCertificate(&cert)
	return nil
}

// HasTLSCertificate reports whether a server certificate is currently installed.
func (ss *StreamServer) HasTLSCertificate() bool { return ss.tlsCert.Load() != nil }

// HTTPSPort returns the active HTTPS port, or 0 when TLS is disabled.
func (ss *StreamServer) HTTPSPort() int { return ss.httpsPort }

// SetFFmpegPath sets the ffmpeg binary used by /thumbnail to extract single
// frames on demand. Call before Listen(); empty leaves thumbnails disabled
// (the handler returns 503). Read-only after Listen() — no locking in the handler.
func (ss *StreamServer) SetFFmpegPath(path string) {
	ss.ffmpegPath = path
}

// SetCacheSubtitles toggles write-through caching of extracted WebVTT into the
// hidden ".unarr" sidecar dir next to the media file (library.cache_subtitles,
// default true). Call before Listen(); read-only thereafter.
func (ss *StreamServer) SetCacheSubtitles(on bool) {
	ss.cacheSubtitles = on
}

// SetCacheThumbnails toggles write-through caching of extracted JPEG frames into
// the hidden ".unarr" sidecar dir next to the media file (library.cache_thumbnails,
// default true). Call before Listen(); read-only thereafter.
func (ss *StreamServer) SetCacheThumbnails(on bool) {
	ss.cacheThumbnails = on
}

// SetTrickplayWidth records the tile width used to build the trickplay sprite
// (library.trickplay.width). 0 leaves trickplay disabled. Call before Listen().
func (ss *StreamServer) SetTrickplayWidth(width int) {
	ss.trickplayWidth = width
}

// SetCORSAllowedOrigins replaces the operator-supplied extra origins. The
// default allowlist (torrentclaw.com / app.torrentclaw.com / localhost dev
// ports) is always merged in. Call before Listen().
func (ss *StreamServer) SetCORSAllowedOrigins(origins []string) {
	ss.corsExtraOrigins = origins
}

// writeCORSHeaders writes the per-origin CORS response headers when the
// request carries an Origin header that matches the allowlist. Returns true
// if the handler must short-circuit (preflight OPTIONS). Media-tag requests
// (no Origin header) bypass this entirely.
//
// `Vary: Origin` is emitted whenever an Origin header is present (matched
// or not) so any intermediate cache keys the response per-origin and a
// later request with a different origin cannot be served a stale ACAO.
func (ss *StreamServer) writeCORSHeaders(w http.ResponseWriter, r *http.Request, expose string) (preflight bool) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	w.Header().Add("Vary", "Origin")
	if _, ok := ss.corsAllowlist[origin]; !ok {
		// Unknown origin — do not emit CORS headers so the browser blocks
		// the response. Still return without short-circuiting so a non-CORS
		// caller (e.g. curl) keeps working.
		return false
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Range")
	if expose != "" {
		w.Header().Set("Access-Control-Expose-Headers", expose)
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

// HLS returns the HLS session registry for this server. Daemon code uses it
// to register a session when the backend asks for HLS playback.
func (ss *StreamServer) HLS() *HLSSessionRegistry { return ss.hls }

// Listen starts the HTTP server on the configured port. Call once at daemon startup.
func (ss *StreamServer) Listen(ctx context.Context) error {
	// Freeze the CORS allowlist before the first request can land. After
	// this point the map is treated as read-only so handlers can probe it
	// without locking.
	ss.corsAllowlist = buildCORSAllowlist(ss.corsExtraOrigins)

	mux := http.NewServeMux()
	mux.HandleFunc("/stream", ss.handler)
	mux.HandleFunc("/health", ss.healthHandler)
	mux.HandleFunc("/speedtest", ss.speedtestHandler)
	mux.HandleFunc("/playlist.m3u", ss.playlistHandler)
	mux.HandleFunc("/hls/", ss.hlsHandler)
	mux.HandleFunc("/thumbnail", ss.thumbnailHandler)
	mux.HandleFunc("/trickplay", ss.trickplayHandler)
	mux.HandleFunc("/sub", ss.subtitleHandler)

	// SO_REUSEADDR allows immediate rebind if the port is in TIME_WAIT (e.g. after agent restart)
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				_ = setReuseAddr(fd)
			})
		},
	}

	// Try configured port; if busy, try next ports (heartbeat reports actual port to web)
	var listener net.Listener
	var listenErr error
	basePort := ss.port
	for attempt := 0; attempt < 10; attempt++ {
		addr := fmt.Sprintf("0.0.0.0:%d", ss.port)
		listener, listenErr = lc.Listen(ctx, "tcp", addr)
		if listenErr == nil {
			break
		}
		if !strings.Contains(listenErr.Error(), "address already in use") {
			return fmt.Errorf("stream server listen on %s: %w", addr, listenErr)
		}
		ss.port++
		log.Printf("[stream] port %d in use, trying %d", ss.port-1, ss.port)
	}
	if listenErr != nil {
		return fmt.Errorf("stream server: all ports busy (%d-%d): %w", basePort, ss.port, listenErr)
	}
	if ss.port != basePort {
		log.Printf("[stream] using port %d (configured %d was busy)", ss.port, basePort)
	}

	ss.port = listener.Addr().(*net.TCPAddr).Port

	// Collect all reachable URLs by network type
	if lanIP := LanIP(); lanIP != "" {
		ss.urls.LAN = fmt.Sprintf("http://%s:%d/stream", lanIP, ss.port)
	}
	if tsIP := TailscaleIP(); tsIP != "" {
		ss.urls.Tailscale = fmt.Sprintf("http://%s:%d/stream", tsIP, ss.port)
	}
	if ss.enableUPnP {
		mapping, err := SetupUPnP(ss.port)
		if err != nil {
			log.Printf("[stream] UPnP setup failed: %v (only LAN/Tailscale clients will reach port %d)", err, ss.port)
		} else {
			ss.upnpMapping = mapping
			ss.urls.Public = fmt.Sprintf("http://%s:%d/stream", mapping.ExternalIP, mapping.ExternalPort)
		}
	} else {
		log.Printf("[stream] UPnP disabled — port %d not published to WAN (set downloads.enable_upnp = true to opt in)", ss.port)
	}

	// Best single URL for backward compat: Tailscale > LAN > Public > localhost
	switch {
	case ss.urls.Tailscale != "":
		ss.url = ss.urls.Tailscale
	case ss.urls.LAN != "":
		ss.url = ss.urls.LAN
	case ss.urls.Public != "":
		ss.url = ss.urls.Public
	default:
		ss.url = fmt.Sprintf("http://127.0.0.1:%d/stream", ss.port)
		ss.urls.LAN = ss.url
	}

	ss.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		if err := ss.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Printf("stream server error: %v", err)
		}
	}()

	log.Printf("[stream] server listening on port %d", ss.port)

	// Optional HTTPS listener (agent-TLS feature). Non-fatal: if it can't bind,
	// HTTP keeps serving so the funnel + LAN HTTP path are unaffected.
	if ss.httpsPort > 0 {
		if err := ss.listenTLS(ctx, mux); err != nil {
			log.Printf("[stream] HTTPS listener disabled: %v", err)
			ss.httpsPort = 0
		}
	}
	return nil
}

// listenTLS starts the HTTPS listener on ss.httpsPort serving the same mux as
// the HTTP server. The certificate is read per-handshake from the atomic holder
// (tlsCert) so a renewed cert applies without restarting the listener; until a
// cert is installed, handshakes fail cleanly (the HTTP path is unaffected).
func (ss *StreamServer) listenTLS(ctx context.Context, mux http.Handler) error {
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) { _ = setReuseAddr(fd) })
		},
	}

	var listener net.Listener
	var err error
	basePort := ss.httpsPort
	for attempt := 0; attempt < 10; attempt++ {
		listener, err = lc.Listen(ctx, "tcp", fmt.Sprintf("0.0.0.0:%d", ss.httpsPort))
		if err == nil {
			break
		}
		if !strings.Contains(err.Error(), "address already in use") {
			return fmt.Errorf("https listen on %d: %w", ss.httpsPort, err)
		}
		ss.httpsPort++
	}
	if err != nil {
		return fmt.Errorf("https: all ports busy (%d-%d): %w", basePort, ss.httpsPort, err)
	}
	ss.httpsPort = listener.Addr().(*net.TCPAddr).Port

	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			if cert := ss.tlsCert.Load(); cert != nil {
				return cert, nil
			}
			return nil, fmt.Errorf("no TLS certificate installed")
		},
	}
	ss.httpsServer = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig:         tlsCfg,
	}

	go func() {
		// Empty cert/key paths → ServeTLS uses TLSConfig.GetCertificate.
		if err := ss.httpsServer.ServeTLS(listener, "", ""); err != nil && err != http.ErrServerClosed {
			log.Printf("[stream] HTTPS server error: %v", err)
		}
	}()

	log.Printf("[stream] HTTPS listening on port %d (certificate installed: %v)", ss.httpsPort, ss.HasTLSCertificate())
	return nil
}

// SetFile atomically swaps the file being served and resets progress tracking.
func (ss *StreamServer) SetFile(provider FileProvider, taskID string) {
	ss.mu.Lock()
	prevGrowing := ss.growing
	ss.provider = provider
	ss.growing = nil // a raw-file provider supersedes any in-flight remux
	ss.taskID = taskID
	ss.mu.Unlock()
	if prevGrowing != nil {
		_ = prevGrowing.Close() // stop the orphan ffmpeg + drop its temp file
	}
	ss.totalFileSize.Store(provider.FileSize())
	ss.lastActivity.Store(time.Now().UnixNano())
	ss.maxByteOffset.Store(0)
	ss.topReaderID.Store(0)
	ss.bitrateBps.Store(0)
	ss.durationSec.Store(0)

	// Probe bitrate + duration synchronously so rate-limiting and duration
	// are available before the first HTTP request arrives.
	if dp, ok := provider.(*diskFileProvider); ok {
		pm := probeMediaInfo(dp.path)
		if pm.bitrateBps > 0 {
			ss.bitrateBps.Store(pm.bitrateBps)
			log.Printf("[stream] detected bitrate: %.1f Mbps → throttle at %.1f Mbps",
				float64(pm.bitrateBps)/1e6, float64(pm.bitrateBps)*2/1e6)
		}
		if pm.durationSec > 0 {
			ss.durationSec.Store(pm.durationSec)
		}
	}
}

// SetGrowingFile serves a progressive-remux source on /stream (hueco #3 / 3b):
// ffmpeg `-c copy` mkv→fMP4 to a growing temp file, range-served via
// serveGrowing. Supersedes any prior provider/growing source (single-viewer).
func (ss *StreamServer) SetGrowingFile(src GrowingSource, taskID string) {
	ss.mu.Lock()
	prevGrowing := ss.growing
	ss.growing = src
	ss.provider = nil
	ss.taskID = taskID
	ss.mu.Unlock()
	if prevGrowing != nil {
		_ = prevGrowing.Close()
	}
	ss.totalFileSize.Store(src.EstimatedSize())
	ss.lastActivity.Store(time.Now().UnixNano())
	ss.maxByteOffset.Store(0)
	ss.topReaderID.Store(0)
	// Rate-limit + bitrate tracking are for raw-file playback; the remux pump
	// has its own pacing (ffmpeg copy is I/O-bound), so leave them at zero.
	ss.bitrateBps.Store(0)
	ss.durationSec.Store(0)
}

// ClearFile stops serving any file. Subsequent requests return 404.
func (ss *StreamServer) ClearFile() {
	ss.mu.Lock()
	ss.provider = nil
	prevGrowing := ss.growing
	ss.growing = nil
	ss.taskID = ""
	ss.mu.Unlock()
	if prevGrowing != nil {
		_ = prevGrowing.Close()
	}
	ss.totalFileSize.Store(0)
	ss.maxByteOffset.Store(0)
	ss.topReaderID.Store(0)
	ss.bitrateBps.Store(0)
	ss.durationSec.Store(0)
}

// CurrentTaskID returns the task ID of the file currently being served.
func (ss *StreamServer) CurrentTaskID() string {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	return ss.taskID
}

// HasFile returns true if a file (raw provider or growing remux) is being served.
func (ss *StreamServer) HasFile() bool {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	return ss.provider != nil || ss.growing != nil
}

// URL returns the best single stream URL (backward compat).
// URL returns the best single /stream URL, carrying a `?t=` token when
// enforcement is on. This is what the one-shot `unarr stream` hands to the
// player — and since the best URL is the Tailscale/LAN address (not loopback),
// it must be tokenised or a remote-addressed player would be rejected.
func (ss *StreamServer) URL() string { return ss.tokenizeStreamURL(ss.url) }

// tokenizeStreamURL appends a freshly-minted `?t=<token>` (scope "stream") to a
// /stream URL. No-op when the URL is empty or enforcement is off.
func (ss *StreamServer) tokenizeStreamURL(u string) string {
	if u == "" || !ss.requireToken {
		return u
	}
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + "t=" + mintStreamToken(ss.streamSecret, streamScopeStream, time.Now())
}

// URLsJSON returns all available stream URLs as a JSON string, each carrying a
// freshly-minted `?t=` stream token when enforcement is on. The web reports
// these verbatim to the browser (pass-through), so the token reaches the
// player without any web-side minting.
func (ss *StreamServer) URLsJSON() string {
	b, _ := json.Marshal(ss.tokenizedStreamURLs())
	return string(b)
}

// tokenizedStreamURLs appends a `?t=<token>` (scope "stream") to each non-empty
// /stream URL. No-op when enforcement is off.
func (ss *StreamServer) tokenizedStreamURLs() StreamURLs {
	if !ss.requireToken {
		return ss.urls
	}
	return StreamURLs{
		LAN:       ss.tokenizeStreamURL(ss.urls.LAN),
		Tailscale: ss.tokenizeStreamURL(ss.urls.Tailscale),
		Public:    ss.tokenizeStreamURL(ss.urls.Public),
	}
}

// Port returns the bound port.
func (ss *StreamServer) Port() int { return ss.port }

// IdleSince returns how long since the last HTTP request was received.
func (ss *StreamServer) IdleSince() time.Duration {
	last := ss.lastActivity.Load()
	if last == 0 {
		return 0
	}
	return time.Since(time.Unix(0, last))
}

// Shutdown gracefully stops the HTTP server and removes the UPnP port mapping.
// Call only at daemon shutdown — NOT between file swaps.
func (ss *StreamServer) Shutdown(ctx context.Context) error {
	ss.upnpMapping.Remove()
	if ss.hls != nil {
		ss.hls.CloseAll()
	}
	if ss.httpsServer != nil {
		if err := ss.httpsServer.Shutdown(ctx); err != nil {
			log.Printf("[stream] HTTPS shutdown: %v", err)
		}
	}
	if ss.server != nil {
		return ss.server.Shutdown(ctx)
	}
	return nil
}

// hlsBaseURLs returns the per-network HLS base URLs for a given session.
// The web client picks the first reachable one — same fallback strategy as
// the legacy /stream URLs.
func (ss *StreamServer) hlsBaseURLs(sessionID string) StreamURLs {
	// Token rides as a path segment so the playlists' relative child URIs
	// (video/index.m3u8, seg-N.m4s, subs/…) inherit it via relative resolution.
	base := "/hls/" + sessionID
	if ss.requireToken {
		base += "/" + mintStreamToken(ss.streamSecret, streamScopeHLS(sessionID), time.Now())
	}
	var out StreamURLs
	if ss.urls.LAN != "" {
		out.LAN = strings.Replace(ss.urls.LAN, "/stream", base, 1)
	}
	if ss.urls.Tailscale != "" {
		out.Tailscale = strings.Replace(ss.urls.Tailscale, "/stream", base, 1)
	}
	if ss.urls.Public != "" {
		out.Public = strings.Replace(ss.urls.Public, "/stream", base, 1)
	}
	return out
}

// HLSURLsJSON returns base URLs for an HLS session as a JSON string for the
// session response payload.
func (ss *StreamServer) HLSURLsJSON(sessionID string) string {
	urls := ss.hlsBaseURLs(sessionID)
	b, _ := json.Marshal(urls)
	return string(b)
}

// hlsHandler routes /hls/<sessionID>/<resource> to the matching HLSSession.
//
// Recognised resources:
//
//	master.m3u8                — top-level playlist
//	video/index.m3u8           — video media playlist
//	video/init.mp4             — fMP4 init segment
//	video/seg-<n>.m4s          — video segment
func (ss *StreamServer) hlsHandler(w http.ResponseWriter, r *http.Request) {
	ss.lastActivity.Store(time.Now().UnixNano())

	if ss.writeCORSHeaders(w, r, "Content-Length, Content-Range, Accept-Ranges") {
		return
	}

	rest := strings.TrimPrefix(r.URL.Path, "/hls/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "missing session id", http.StatusNotFound)
		return
	}
	sessionID := parts[0]
	// Reject malformed IDs with the same 404 we return for unknown sessions —
	// no oracle for the accepted format.
	if !validSessionID.MatchString(sessionID) {
		http.Error(w, "hls session not found", http.StatusNotFound)
		return
	}
	remainder := ""
	if len(parts) > 1 {
		remainder = parts[1]
	}
	// Auth: when enforcement is on, the URL is /hls/<sessionID>/<token>/<resource>.
	// Peel the token segment and verify it (no loopback exemption — funnel
	// traffic arrives over localhost). 404 on mismatch — same response as an
	// unknown session, no oracle.
	if ss.requireToken {
		sub := strings.SplitN(remainder, "/", 2)
		if !verifyStreamToken(ss.streamSecret, streamScopeHLS(sessionID), sub[0], time.Now()) {
			http.Error(w, "hls session not found", http.StatusNotFound)
			return
		}
		if len(sub) < 2 {
			http.Error(w, "missing resource", http.StatusNotFound)
			return
		}
		remainder = sub[1]
	}
	session := ss.hls.Get(sessionID)
	if session == nil {
		http.Error(w, "hls session not found", http.StatusNotFound)
		return
	}
	if remainder == "" {
		http.Error(w, "missing resource", http.StatusNotFound)
		return
	}
	resource := remainder

	switch {
	case resource == "master.m3u8":
		session.ServeMaster(w, r)
	case resource == "probe.json":
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		_ = json.NewEncoder(w).Encode(session.ProbeInfo())
	case resource == "video/index.m3u8":
		session.ServeVideoPlaylist(w, r)
	case resource == "video/init.mp4":
		session.ServeInit(w, r)
	case strings.HasPrefix(resource, "video/seg-") && strings.HasSuffix(resource, ".m4s"):
		idxStr := strings.TrimSuffix(strings.TrimPrefix(resource, "video/seg-"), ".m4s")
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			http.Error(w, "bad segment index", http.StatusBadRequest)
			return
		}
		session.ServeSegment(w, r, idx)
	default:
		// Subtitles are no longer served here — the web player fetches each text
		// track on demand from /sub (subtitleHandler). The master playlist no
		// longer advertises a SUBTITLES group, so no player requests subs/sub-*.
		http.Error(w, "unknown hls resource", http.StatusNotFound)
	}
}

// healthHandler responde con el estado del servidor en JSON.
// Útil para diagnosticar conectividad desde redes remotas o Tailscale:
//
//	curl http://<tailscale-ip>:<port>/health
func (ss *StreamServer) healthHandler(w http.ResponseWriter, r *http.Request) {
	if ss.writeCORSHeaders(w, r, "") {
		return
	}
	ss.mu.RLock()
	provider := ss.provider
	taskID := ss.taskID
	ss.mu.RUnlock()

	clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	// Only expose filename/taskID/client to loopback callers (local diagnostics).
	// Remote callers (LAN, Tailscale, UPnP public) get a minimal probe response
	// so that scanners and unauthenticated peers cannot fingerprint the active
	// download. The web stream-probe only checks HTTP 200 + Content-Type.
	//
	// Use net.IP.IsLoopback so we also accept ::ffff:127.0.0.1 (Linux dual-stack
	// IPv4-mapped form) and reject the empty-string fallthrough when
	// SplitHostPort fails on a malformed RemoteAddr — both would otherwise
	// silently bypass the disclosure boundary.
	parsedIP := net.ParseIP(clientIP)
	isLocal := parsedIP != nil && parsedIP.IsLoopback()

	type healthResponse struct {
		Status    string `json:"status"`
		Streaming bool   `json:"streaming"`
		File      string `json:"file,omitempty"`
		Task      string `json:"task,omitempty"`
		Port      int    `json:"port"`
		Client    string `json:"client,omitempty"`
	}
	resp := healthResponse{
		Status: "ok",
		Port:   ss.port,
	}
	if provider != nil {
		resp.Streaming = true
	}
	if isLocal {
		resp.Client = clientIP
		if provider != nil {
			resp.File = provider.FileName()
			resp.Task = taskID
			if len(resp.Task) > 8 {
				resp.Task = resp.Task[:8]
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	json.NewEncoder(w).Encode(resp) //nolint:errcheck
}

// speedtestHandler streams a fixed-size, incompressible payload so the web
// player can measure REAL throughput to THIS agent — the path the stream
// actually travels (LAN-direct, tailnet, or the CF funnel). The web origin's
// link tells us nothing about that path; measuring it here is the only honest
// signal for the pre-play quality suggestion. No auth or active stream needed:
// the bytes carry no information. CORS-gated like the other endpoints so the
// cross-origin fetch can read + time the body.
func (ss *StreamServer) speedtestHandler(w http.ResponseWriter, r *http.Request) {
	ss.lastActivity.Store(time.Now().UnixNano())
	if ss.writeCORSHeaders(w, r, "") {
		return
	}
	// Single-flight: this endpoint is unauthenticated (it carries no data) and
	// reachable over the public cloudflared funnel, so bound the bandwidth a
	// caller can drain — only one measurement runs at a time, a concurrent
	// request gets 429 instead of stacking another multi-MB transfer.
	if !ss.speedtestActive.CompareAndSwap(false, true) {
		http.Error(w, "speedtest busy", http.StatusTooManyRequests)
		return
	}
	defer ss.speedtestActive.Store(false)
	const defaultSize = 2 * 1024 * 1024
	const maxSize = 4 * 1024 * 1024 // matches the web /api/v1/speed-test cap
	size := defaultSize
	if v := r.URL.Query().Get("size"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n < 64*1024 {
				n = 64 * 1024
			} else if n > maxSize {
				n = maxSize
			}
			size = n
		}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(size))
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	// Reuse one non-repeating chunk (incompressible enough that gzip can't skew
	// the measurement) to avoid per-write allocation.
	const chunk = 64 * 1024
	buf := make([]byte, chunk)
	for i := range buf {
		buf[i] = byte((i*31 + 7) & 0xff)
	}
	for remaining := size; remaining > 0; {
		n := chunk
		if remaining < n {
			n = remaining
		}
		if _, err := w.Write(buf[:n]); err != nil {
			return // client disconnected mid-measure
		}
		remaining -= n
	}
}

// playlistHandler generates an M3U playlist for VLC with #EXTVLCOPT language hints.
// Query params: audioLangs (comma-sep), subLangs (comma-sep), resumeSec, title, streamUrl.
// If streamUrl is omitted, uses the current best stream URL.
//
// VLC fetches this playlist and applies the EXTVLCOPT directives automatically,
// enabling automatic audio/subtitle track selection on all VLC platforms (desktop + mobile).
func (ss *StreamServer) playlistHandler(w http.ResponseWriter, r *http.Request) {
	if ss.writeCORSHeaders(w, r, "") {
		return
	}

	q := r.URL.Query()

	// Sanitize query params: strip CR/LF to prevent M3U directive injection.
	sanitize := func(s string) string {
		s = strings.ReplaceAll(s, "\n", "")
		s = strings.ReplaceAll(s, "\r", "")
		return s
	}

	audioLangs := sanitize(q.Get("audioLangs"))
	subLangs := sanitize(q.Get("subLangs"))
	resumeSec := sanitize(q.Get("resumeSec"))
	title := sanitize(q.Get("title"))
	streamURL := q.Get("streamUrl")
	// VLC network buffer (ms). The web sends a network-aware value (small on
	// LAN/Tailscale, larger on the CF funnel); clamp to a sane range. Older web
	// clients that don't pass it get a modest default — the old flat 30000 made
	// VLC pre-buffer ~30 s before playback even on a fast, range-served source.
	networkCaching := 3000
	if v := sanitize(q.Get("networkCaching")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n < 500 {
				n = 500
			} else if n > 60000 {
				n = 60000
			}
			networkCaching = n
		}
	}
	// Only accept http(s) URLs to prevent file:// or other URI schemes in the playlist.
	if streamURL != "" && !strings.HasPrefix(streamURL, "http://") && !strings.HasPrefix(streamURL, "https://") {
		streamURL = ""
	}
	if streamURL == "" {
		// No self-minting fallback: returning a freshly-tokenised URL for a
		// param-less request would make /playlist.m3u an open token oracle
		// (any caller could fetch a valid /stream?t=… here). The web always
		// passes an already-tokenised streamUrl param; the playlist just echoes
		// it — the real auth gate is /stream itself.
		http.Error(w, "no active stream", http.StatusNotFound)
		return
	}
	if title == "" {
		title = "TorrentClaw Stream"
	}

	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString(fmt.Sprintf("#EXTINF:-1,%s\n", title))
	if audioLangs != "" {
		b.WriteString(fmt.Sprintf("#EXTVLCOPT:audio-language=%s\n", audioLangs))
	}
	if subLangs != "" {
		b.WriteString(fmt.Sprintf("#EXTVLCOPT:sub-language=%s\n", subLangs))
	}
	if resumeSec != "" && resumeSec != "0" {
		b.WriteString(fmt.Sprintf("#EXTVLCOPT:start-time=%s\n", resumeSec))
	}
	b.WriteString(fmt.Sprintf("#EXTVLCOPT:network-caching=%d\n", networkCaching))
	b.WriteString(streamURL + "\n")

	w.Header().Set("Content-Type", "audio/x-mpegurl")
	w.Header().Set("Content-Disposition", `inline; filename="stream.m3u"`)
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprint(w, b.String()) //nolint:errcheck
}

func (ss *StreamServer) handler(w http.ResponseWriter, r *http.Request) {
	ss.lastActivity.Store(time.Now().UnixNano())

	// Log every incoming request — essential for diagnosing remote/Tailscale issues.
	clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	log.Printf("[stream] %s /stream from %s Range:%q", r.Method, clientIP, r.Header.Get("Range"))

	// Get current source (raw provider or growing remux; nil if none).
	ss.mu.RLock()
	provider := ss.provider
	growing := ss.growing
	ss.mu.RUnlock()

	if provider == nil && growing == nil {
		http.Error(w, "no active stream", http.StatusNotFound)
		return
	}

	if ss.writeCORSHeaders(w, r, "Content-Length, Content-Range, Accept-Ranges") {
		return
	}

	// Auth: every caller must carry a valid stream token. 404 (not 401/403) so
	// an unauthorised caller gets no oracle that a stream is active here.
	if !ss.checkStreamToken(streamScopeStream, r.URL.Query().Get("t")) {
		log.Printf("[stream] rejected %s — bad/absent token", clientIP)
		http.Error(w, "no active stream", http.StatusNotFound)
		return
	}

	// Progressive-remux path (3b): a growing fMP4 produced by ffmpeg `-c copy`.
	// Range-served manually because http.ServeContent needs a complete file.
	if growing != nil {
		ss.serveGrowing(w, r, growing)
		return
	}

	rawReader := provider.NewFileReader(r.Context())
	if rawReader == nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	defer rawReader.Close()

	// Wrap reader to track bytes read for progress estimation + rate limit.
	// Rate limiting at ~2x bitrate ensures VLC can't download far ahead of
	// playback, so bytes-read ≈ playback position (like Netflix/YouTube).
	bps := ss.bitrateBps.Load()
	var bytesPerSec int64
	if bps > 0 {
		bytesPerSec = bps / 8 * 2 // 2x bitrate in bytes/sec
	}
	var burstSize int64
	if bytesPerSec > 0 {
		burstSize = bytesPerSec * 30
	}
	reader := &trackingReader{
		inner:       rawReader,
		server:      ss,
		id:          ss.readerCounter.Add(1),
		bytesPerSec: bytesPerSec,
		burstSize:   burstSize,
		tokens:      burstSize,
		lastFill:    time.Now(),
	}

	w.Header().Set("Content-Type", mimeTypeFromExt(provider.FileName()))
	// "inline" for play requests (VLC/mpv), "attachment" for download requests.
	disposition := "inline"
	if r.URL.Query().Get("download") == "1" {
		disposition = "attachment"
	}
	downloadName := provider.FileName()
	if disposition == "attachment" {
		ext := filepath.Ext(downloadName)
		downloadName = strings.TrimSuffix(downloadName, ext) + " [TorrentClaw]" + ext
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("%s; filename=%q", disposition, downloadName))
	w.Header().Set("Accept-Ranges", "bytes")

	http.ServeContent(w, r, provider.FileName(), time.Time{}, reader)
}

// thumbnailHandler serves ONE JPEG frame decoded from a file at a timestamp.
// It backs the web's "file characteristics" panel (frames on demand, hueco
// medio): the panel renders a strip of <img> at several positions, each hitting
// this route. Independent of the active /stream — no session, no provider, no
// effect on playback; ffmpeg just seeks the path and emits a single frame.
//
// Auth: a token scoped thumb:<sha256(path)> minted by the web with this agent's
// stream secret. The path travels in ?p= (already client-visible — the library
// UI shows it) and the token's scope binds that exact path, so a tampered p
// fails verification. 404 (not 401/403) on a bad token — no oracle, same as
// /stream. The path is additionally clamped to a real regular file as
// defense-in-depth against a (trusted) web bug pointing ffmpeg at a device/FIFO.
func (ss *StreamServer) thumbnailHandler(w http.ResponseWriter, r *http.Request) {
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
	if !ss.checkStreamToken(streamScopeThumb(rawPath), q.Get("t")) {
		clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)
		log.Printf("[thumbnail] rejected from %s — bad/absent token", clientIP)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if fi, err := os.Stat(rawPath); err != nil || !fi.Mode().IsRegular() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	pos := parseThumbPos(q.Get("pos"))
	width := parseThumbWidth(q.Get("w"))

	// Cache hit: serve a fresh sidecar (written by the scan-time prewarm — which
	// pre-extracts the 10/30/50/70/90% panel frames — or a prior request),
	// skipping ffmpeg. Checked BEFORE the ffmpeg guard so a pre-warmed frame is
	// still serveable even if ffmpeg was removed after the cache was filled.
	if jpeg, ok := mediainfo.ReadCachedThumbnail(rawPath, pos, width); ok {
		ss.writeJPEG(w, jpeg)
		return
	}

	// Beyond here we must extract on demand, which needs ffmpeg.
	if ss.ffmpegPath == "" {
		http.Error(w, "thumbnails unavailable", http.StatusServiceUnavailable)
		return
	}

	// Cap the work: a single keyframe decode is fast, but a corrupt/huge file or
	// a seek past EOF could hang ffmpeg. 20s is generous for a keyframe seek.
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, ss.ffmpegPath, buildThumbnailArgs(rawPath, pos, width)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		// Fast input-seek (-ss before -i) can fail on files whose seek index is
		// imprecise or mildly corrupt: the demuxer lands mid-EBML element
		// ("invalid as first byte of an EBML number") and no frame decodes.
		// Retry once with the slow but robust output-seek path before giving up
		// (2026-06-03: anime MKVs returned a broken image in the web scrubber).
		log.Printf("[thumbnail] input-seek failed (pos=%.1f w=%d path=%q): err=%v %s — retrying output-seek",
			pos, width, rawPath, err, strings.TrimSpace(stderr.String()))
		var stderr2 strings.Builder
		cmd2 := exec.CommandContext(ctx, ss.ffmpegPath, buildThumbnailArgsAccurate(rawPath, pos, width)...)
		cmd2.Stderr = &stderr2
		out, err = cmd2.Output()
		if err != nil || len(out) == 0 {
			log.Printf("[thumbnail] no frame after output-seek fallback (pos=%.1f w=%d path=%q): err=%v %s",
				pos, width, rawPath, err, strings.TrimSpace(stderr2.String()))
			http.Error(w, "thumbnail failed", http.StatusInternalServerError)
			return
		}
	}
	// Write-through so the next request (and trickplay re-hover) is a cache hit.
	if ss.cacheThumbnails {
		if werr := mediainfo.WriteCachedThumbnail(rawPath, pos, width, out); werr != nil {
			log.Printf("[thumbnail] cache write skipped (pos=%.1f w=%d path=%q): %v", pos, width, rawPath, werr)
		}
	}
	ss.writeJPEG(w, out)
}

// writeJPEG writes the standard single-frame response headers + body for both
// the cache-hit and freshly-extracted paths of thumbnailHandler.
func (ss *StreamServer) writeJPEG(w http.ResponseWriter, jpeg []byte) {
	w.Header().Set("Content-Type", "image/jpeg")
	// path+pos is stable content; let the browser cache so re-opening the panel
	// doesn't re-fetch. private — it's a frame of the user's own file.
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("Content-Length", strconv.Itoa(len(jpeg)))
	if _, err := w.Write(jpeg); err != nil {
		log.Printf("[thumbnail] write failed: %v", err)
	}
}

// trickplayHandler serves the pre-built trickplay montage sprite (kind=sprite →
// JPEG) or its manifest (default → JSON) for a file. The sprite is generated by
// the scan-time prewarm (library.trickplay) so playback does NO live extraction
// (no contention with the active stream — the cause of broken seekbar previews).
// The agent owns the tile width (its config), so the web requests by path only
// and reads geometry from the manifest. Auth mirrors /thumbnail (a
// thumb:<sha256(path)> token). 404 when no sprite exists yet → the web falls
// back to on-demand /thumbnail.
func (ss *StreamServer) trickplayHandler(w http.ResponseWriter, r *http.Request) {
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
	if !ss.checkStreamToken(streamScopeThumb(rawPath), q.Get("t")) {
		clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)
		log.Printf("[trickplay] rejected from %s — bad/absent token", clientIP)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if ss.trickplayWidth <= 0 {
		http.Error(w, "trickplay disabled", http.StatusNotFound)
		return
	}
	if fi, err := os.Stat(rawPath); err != nil || !fi.Mode().IsRegular() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	manifest, ok := mediainfo.ReadCachedTrickplay(rawPath, ss.trickplayWidth)
	if !ok {
		http.Error(w, "trickplay not available", http.StatusNotFound)
		return
	}
	if q.Get("kind") == "sprite" {
		f, err := os.Open(mediainfo.TrickplaySpritePath(rawPath, ss.trickplayWidth))
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		defer f.Close()
		mod := time.Time{}
		if fi, serr := f.Stat(); serr == nil {
			mod = fi.ModTime()
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "private, max-age=3600")
		http.ServeContent(w, r, "trickplay.jpg", mod, f)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	if err := json.NewEncoder(w).Encode(manifest); err != nil {
		log.Printf("[trickplay] manifest encode failed: %v", err)
	}
}

// subtitleHandler extracts ONE embedded TEXT subtitle stream from a file and
// serves it as WebVTT, on demand. It's the single subtitle source the web
// player uses for BOTH direct-play and HLS (attached as an external <track>),
// so subtitles are identical regardless of play method or whether playback runs
// natively or via hls.js — no longer dependent on the browser's HLS engine
// surfacing in-manifest renditions.
//
// Mirrors thumbnailHandler: path in ?p= (client-visible), index in ?i=, and the
// token scope binds path+index so a tampered p/i fails verification. 404 on a
// bad token (no oracle). The path is clamped to a regular file as defense in
// depth. Bitmap subs (PGS/DVB) have no text form — those are burned in via the
// HLS path and are not served here; the web only requests text tracks.
func (ss *StreamServer) subtitleHandler(w http.ResponseWriter, r *http.Request) {
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
	if !ss.checkStreamToken(streamScopeSub(rawPath, index), q.Get("t")) {
		clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)
		log.Printf("[sub] rejected from %s — bad/absent token", clientIP)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if fi, statErr := os.Stat(rawPath); statErr != nil || !fi.Mode().IsRegular() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Cache hit: serve a fresh sidecar (written by the scan-time prewarm or a
	// prior request) instantly, skipping ffmpeg. This is also what makes huge
	// remuxes work — the prewarm extracts without the on-demand HTTP timeout
	// below, so by play time the hit avoids the 60s ceiling that was returning
	// 500s on 50GB+ files. Checked BEFORE the ffmpeg guard so a pre-warmed track
	// is still serveable even if ffmpeg was removed after the cache was filled.
	if vtt, ok := mediainfo.ReadCachedSubtitle(rawPath, index); ok {
		ss.writeVTT(w, vtt)
		return
	}

	// Beyond here we must extract on demand, which needs ffmpeg.
	if ss.ffmpegPath == "" {
		http.Error(w, "subtitles unavailable", http.StatusServiceUnavailable)
		return
	}

	// A full subtitle track is small (KBs–low MBs); 60s is ample for a normal
	// movie's text track and bounds a hung/corrupt ffmpeg. Giant remuxes can
	// exceed this on first play — the prewarm pre-fills the cache so this
	// on-demand path is the fallback, not the steady state.
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	out, err := mediainfo.ExtractSubtitleVTT(ctx, ss.ffmpegPath, rawPath, index)
	if err != nil {
		log.Printf("[sub] extract failed (i=%d path=%q): %v", index, rawPath, err)
		http.Error(w, "subtitle extract failed", http.StatusInternalServerError)
		return
	}
	// Write-through so the next request is a cache hit. Best-effort: a read-only
	// media mount just logs and serves the in-memory bytes.
	if ss.cacheSubtitles {
		if werr := mediainfo.WriteCachedSubtitle(rawPath, index, out); werr != nil {
			log.Printf("[sub] cache write skipped (i=%d path=%q): %v", index, rawPath, werr)
		}
	}
	ss.writeVTT(w, out)
}

// writeVTT writes the standard WebVTT response headers + body for both the
// cache-hit and freshly-extracted paths of subtitleHandler.
func (ss *StreamServer) writeVTT(w http.ResponseWriter, vtt []byte) {
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	// path+index is stable content for the daemon's lifetime; let the browser
	// cache so re-selecting a track doesn't re-fetch. private — the user's file.
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("Content-Length", strconv.Itoa(len(vtt)))
	//nolint:gosec // G705: WebVTT served as text/vtt to a <track> element — not
	// HTML, so cue text can't execute; the path is token-scoped + stat'd as a
	// regular file, and ffmpeg only emits well-formed WebVTT.
	if _, err := w.Write(vtt); err != nil {
		log.Printf("[sub] write failed: %v", err)
	}
}

// buildThumbnailArgs builds the ffmpeg argv that decodes ONE frame at posSec and
// writes a scaled JPEG to stdout. `-ss` BEFORE `-i` does an input (keyframe)
// seek — near-constant time regardless of position — instead of decoding from
// the start. scale=w:-2 preserves aspect with an even height (mjpeg/yuv420
// requires even dimensions). `-an -sn` drops audio/subtitle streams.
func buildThumbnailArgs(path string, posSec float64, width int) []string {
	return []string{
		"-nostdin",
		"-loglevel", "error",
		"-ss", strconv.FormatFloat(posSec, 'f', 3, 64),
		"-i", path,
		"-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:-2", width),
		"-an", "-sn",
		"-f", "mjpeg",
		"pipe:1",
	}
}

// buildThumbnailArgsAccurate is the robust fallback for files whose seek index
// is imprecise or mildly corrupt, where the fast input seek (-ss before -i)
// lands mid-EBML element and decodes no frame. `-ss` AFTER `-i` is an output
// (decode) seek — slower (decodes from the start) but reliable — and
// `-err_detect ignore_err` tolerates minor stream corruption encountered along
// the way. Only used after buildThumbnailArgs fails, so its extra cost is paid
// solely for files the fast path can't handle.
func buildThumbnailArgsAccurate(path string, posSec float64, width int) []string {
	return []string{
		"-nostdin",
		"-loglevel", "error",
		"-err_detect", "ignore_err",
		"-i", path,
		"-ss", strconv.FormatFloat(posSec, 'f', 3, 64),
		"-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:-2", width),
		"-an", "-sn",
		"-f", "mjpeg",
		"pipe:1",
	}
}

// parseThumbPos parses a non-negative seconds offset; defaults to 0 on garbage.
func parseThumbPos(s string) float64 {
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
}

// parseThumbWidth parses the requested width, defaulting to 320 and clamping to
// [80, 640] so a caller can't ask ffmpeg to upscale to an absurd size.
func parseThumbWidth(s string) int {
	const def, min, max = 320, 80, 640
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// serveGrowing range-serves a growing remux source (hueco #3 / 3b). Unlike
// http.ServeContent it can't rely on a fixed file size: ffmpeg `-c copy` is
// still writing, and the final byte count isn't known until it exits. So we:
//
//   - advertise an ESTIMATED total (≈ source file size for a copy remux) in
//     Content-Range so the browser scrubber has a timeline;
//   - reply 206 and stream from the requested offset, blocking via ReadAt for
//     not-yet-produced bytes, until the explicit range end or the real EOF;
//   - send the body chunked (no Content-Length) for non-final sources, since
//     the true length differs from the estimate — promising an exact length we
//     can't fulfil would hang the browser. When the source is already final we
//     send an exact Content-Length.
//
// Seeking forward into a not-yet-remuxed region blocks briefly until the copy
// (I/O-bound, fast) catches up; seeking back to produced bytes is immediate.
func (ss *StreamServer) serveGrowing(w http.ResponseWriter, r *http.Request, src GrowingSource) {
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", src.FileName()))

	// Total to advertise: exact when ffmpeg has exited, else the estimate.
	total := src.EstimatedSize()
	if src.Final() {
		total = src.Size()
	}
	if total <= 0 {
		total = src.Size()
	}

	start, explicitEnd := parseByteRange(r.Header.Get("Range"))
	if total > 0 && start >= total {
		// Range beyond what we expect to produce — let the browser recover.
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", total))
		http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return
	}

	if r.Method == http.MethodHead {
		if total > 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	end := total - 1
	if explicitEnd >= 0 && explicitEnd < end {
		end = explicitEnd
	}
	if total > 0 {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
	}
	// Exact Content-Length only when the source is final (true size known) so
	// we never promise bytes a still-running remux might not produce.
	if src.Final() && explicitEnd < 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(src.Size()-start, 10))
	}
	w.WriteHeader(http.StatusPartialContent)

	buf := make([]byte, 256*1024)
	off := start
	firstRead := true
	for {
		if explicitEnd >= 0 && off > explicitEnd {
			return
		}
		if r.Context().Err() != nil {
			return // client disconnected / request cancelled
		}
		readStart := time.Now()
		n, err := src.ReadAt(buf, off)
		// TTFF diagnosis: a read that blocks means the client asked for bytes the
		// remux hasn't produced yet (a seek ahead of the live edge, or the very
		// first read before ffmpeg's init lands). Log it so a slow start is
		// attributable to "waiting on ffmpeg" vs network/decoder.
		if waited := time.Since(readStart); waited > 250*time.Millisecond {
			log.Printf("[stream] serveGrowing read off=%d blocked %v (produced=%d est=%d)",
				off, waited.Round(time.Millisecond), src.Size(), src.EstimatedSize())
		} else if firstRead {
			log.Printf("[stream] serveGrowing start off=%d (produced=%d est=%d)", start, src.Size(), src.EstimatedSize())
		}
		firstRead = false
		if n > 0 {
			toWrite := n
			if explicitEnd >= 0 {
				if remaining := explicitEnd - off + 1; int64(toWrite) > remaining {
					toWrite = int(remaining)
				}
			}
			if _, werr := w.Write(buf[:toWrite]); werr != nil {
				return // client gone
			}
			off += int64(toWrite)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if err != nil {
			// transcodeSource returns io.EOF only at the true (final) end; any
			// other error means ffmpeg failed or the read timed out. Either
			// way the stream is over — close the body.
			return
		}
	}
}

// parseByteRange parses a single "bytes=start-[end]" header into (start, end).
// end is -1 when open-ended or absent. Multi-range and suffix ranges
// ("bytes=-N") are not supported (returns start=0) — the browser falls back to
// a normal open-ended request, which is all <video> needs for a growing source.
func parseByteRange(header string) (start, end int64) {
	end = -1
	if !strings.HasPrefix(header, "bytes=") {
		return 0, -1
	}
	spec := strings.TrimPrefix(header, "bytes=")
	if i := strings.IndexByte(spec, ','); i >= 0 {
		spec = spec[:i] // first range only
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, -1
	}
	startStr := strings.TrimSpace(spec[:dash])
	if startStr == "" {
		// Suffix range "bytes=-N" (last N bytes) is unsupported on a growing
		// source whose total isn't fixed — serve open-ended from 0 instead of
		// mis-reading N as an absolute end. fMP4 (moov at front) never needs it.
		return 0, -1
	}
	if v, err := strconv.ParseInt(startStr, 10, 64); err == nil && v >= 0 {
		start = v
	}
	if e := strings.TrimSpace(spec[dash+1:]); e != "" {
		if v, err := strconv.ParseInt(e, 10, 64); err == nil && v >= 0 {
			end = v
		}
	}
	return start, end
}

// EstimatedProgress returns estimated watch progress percentage (0-100)
// and the total duration in seconds (0 if unknown).
func (ss *StreamServer) EstimatedProgress() (pct int, durationSec int) {
	total := ss.totalFileSize.Load()
	if total <= 0 {
		return 0, 0
	}
	maxOffset := ss.maxByteOffset.Load()
	p := int(float64(maxOffset) / float64(total) * 100)
	if p > 100 {
		p = 100
	}
	return p, int(ss.durationSec.Load())
}

// --- File Providers ---

// NewDiskFileProvider creates a FileProvider that serves a file from disk.
func NewDiskFileProvider(filePath string) FileProvider {
	return &diskFileProvider{
		path: filePath,
		name: filepath.Base(filePath),
	}
}

// diskFileProvider serves a file from disk.
type diskFileProvider struct {
	path string
	name string
}

func (p *diskFileProvider) NewFileReader(_ context.Context) io.ReadSeekCloser {
	f, err := os.Open(p.path)
	if err != nil {
		log.Printf("[stream] failed to open %q: %v", p.path, err)
		return nil
	}
	return f
}

func (p *diskFileProvider) FileName() string { return p.name }

func (p *diskFileProvider) FileSize() int64 {
	fi, err := os.Stat(p.path)
	if err != nil {
		log.Printf("[stream] failed to stat %q: %v", p.path, err)
		return 0
	}
	return fi.Size()
}

// NewTorrentFileProvider creates a FileProvider from an active torrent file.
// dataDir locates the on-disk file for a best-effort bitrate probe that sizes
// the streaming readahead. The probe runs ASYNC so stream start never blocks on
// ffprobe (a missing header would otherwise stall up to the probe timeout);
// until it resolves, readers use the default window, and readers created after
// it resolves pick up the accurate size.
func NewTorrentFileProvider(file *torrent.File, dataDir string) FileProvider {
	p := &torrentFileProvider{file: file}
	if dataDir != "" {
		go func() {
			if bps := probeMediaInfo(filepath.Join(dataDir, file.DisplayPath())).bitrateBps; bps > 0 {
				p.bitrateBps.Store(bps)
			}
		}()
	}
	return p
}

// torrentFileProvider wraps a torrent.File to implement FileProvider.
type torrentFileProvider struct {
	file *torrent.File
	// bitrateBps sizes the reader's readahead window (see dynamicReadahead).
	// Set asynchronously by the bitrate probe; 0 until then → default window.
	bitrateBps atomic.Int64
}

func (p *torrentFileProvider) NewFileReader(ctx context.Context) io.ReadSeekCloser {
	reader := p.file.NewReader()
	reader.SetResponsive()
	// Bitrate-sized window (vs the old static 5 MiB that stalled HD/4K). anacrolix
	// prioritises pieces in this window ahead of the read position + on seek.
	reader.SetReadahead(dynamicReadahead(p.bitrateBps.Load()))
	reader.SetContext(ctx)
	return reader
}

func (p *torrentFileProvider) FileName() string {
	return filepath.Base(p.file.DisplayPath())
}

func (p *torrentFileProvider) FileSize() int64 {
	return p.file.Length()
}

// --- Utility functions ---

// FindVideoFile scans a directory (recursively) for the largest video file.
func FindVideoFile(dir string) string {
	var best string
	var bestSize int64

	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if !VideoExts[ext] {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Size() > bestSize {
			best = path
			bestSize = info.Size()
		}
		return nil
	})
	return best
}

// LanIP returns the machine's LAN IP, or "" if unavailable.
func LanIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

// TailscaleIP returns the Tailscale IPv4 address, or "" if Tailscale isn't running.
func TailscaleIP() string {
	out, err := exec.Command("tailscale", "ip", "-4").Output()
	if err != nil {
		return ""
	}
	ip := strings.TrimSpace(string(out))
	if net.ParseIP(ip) == nil {
		return ""
	}
	return ip
}

// trackingReader wraps an io.ReadSeekCloser with:
//   - Progress tracking: atomically updates maxByteOffset on Read (not Seek).
//   - Rate limiting: token bucket throttle at ~2x video bitrate so that
//     bytes-read ≈ playback position. Without this, local/NAS files get
//     downloaded instantly and progress jumps to 100%.
//
// Rate limiting happens AFTER each Read (sleep to pace), never before.
// This ensures the client always receives data and never times out.
type trackingReader struct {
	inner       io.ReadSeekCloser
	server      *StreamServer
	id          int64 // unique ID for this reader
	pos         int64 // current read position
	bytesRead   int64 // total bytes read by THIS connection (measures sequential progress)
	bytesPerSec int64 // 0 = unlimited (remote/torrent), >0 = throttled (local disk)

	// Token bucket state
	tokens    int64     // available bytes to serve (can go negative = we're ahead)
	lastFill  time.Time // last time tokens were replenished
	burstSize int64     // max token accumulation (caps how far ahead VLC can buffer)
}

func (t *trackingReader) Read(p []byte) (int, error) {
	// Always read immediately — never block before serving data to the client.
	n, err := t.inner.Read(p)
	if n > 0 {
		t.pos += int64(n)
		t.bytesRead += int64(n)

		// Only the reader that has read the most bytes can update progress.
		// This prevents VLC's metadata/index requests (which read near EOF)
		// from inflating progress to 100%.
		if t.server.topReaderID.Load() == t.id {
			// We own the progress — advance it (never regress)
			for {
				cur := t.server.maxByteOffset.Load()
				if t.pos <= cur || t.server.maxByteOffset.CompareAndSwap(cur, t.pos) {
					break
				}
			}
		} else {
			// Try to take over if we've read more than the current progress.
			// CAS loop prevents two goroutines from interleaving their stores.
			for {
				cur := t.server.maxByteOffset.Load()
				if t.bytesRead <= cur {
					break
				}
				if t.server.maxByteOffset.CompareAndSwap(cur, t.pos) {
					t.server.topReaderID.Store(t.id)
					break
				}
			}
		}

		// Rate limit: sleep AFTER read to pace throughput.
		if t.bytesPerSec > 0 {
			t.fillTokens()
			t.tokens -= int64(n)
			if t.tokens < 0 {
				deficit := -t.tokens
				sleepNs := (deficit * int64(time.Second)) / t.bytesPerSec
				if sleepNs > int64(time.Second) {
					sleepNs = int64(time.Second)
				}
				time.Sleep(time.Duration(sleepNs))
			}
		}
	}
	return n, err
}

func (t *trackingReader) Seek(offset int64, whence int) (int64, error) {
	newPos, err := t.inner.Seek(offset, whence)
	if err == nil {
		t.pos = newPos
		// Don't update maxByteOffset on Seek — http.ServeContent seeks to EOF
		// to determine size, which would instantly mark progress as 100%.
		// Don't reset tokens — prevents clients from bypassing rate limiting
		// by issuing repeated seeks to refill the token bucket.
	}
	return newPos, err
}

func (t *trackingReader) Close() error { return t.inner.Close() }

func (t *trackingReader) fillTokens() {
	now := time.Now()
	elapsed := now.Sub(t.lastFill)
	if elapsed <= 0 {
		return
	}
	newTokens := int64(elapsed.Seconds() * float64(t.bytesPerSec))
	t.tokens += newTokens
	if t.tokens > t.burstSize {
		t.tokens = t.burstSize
	}
	t.lastFill = now
}

// probeMedia holds bitrate and duration extracted by ffprobe.
type probeMedia struct {
	bitrateBps  int64 // bits per second
	durationSec int64 // seconds
}

// probeBitrate uses ffprobe to detect the video bitrate and duration.
// Returns zero values if ffprobe is not available or the file can't be probed.
func probeMediaInfo(filePath string) probeMedia {
	// Defense-in-depth: only probe regular files (not FIFOs, devices, etc.)
	if fi, err := os.Stat(filePath); err != nil || !fi.Mode().IsRegular() {
		return probeMedia{}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "ffprobe",
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		filePath,
	).Output()
	if err != nil {
		return probeMedia{}
	}

	var result struct {
		Format struct {
			BitRate  string `json:"bit_rate"`
			Duration string `json:"duration"`
			Size     string `json:"size"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return probeMedia{}
	}

	var pm probeMedia

	// Parse duration
	if result.Format.Duration != "" {
		dur, _ := strconv.ParseFloat(result.Format.Duration, 64)
		if dur > 0 {
			pm.durationSec = int64(dur)
		}
	}

	// Prefer explicit bit_rate from ffprobe
	if result.Format.BitRate != "" {
		bps, _ := strconv.ParseInt(result.Format.BitRate, 10, 64)
		if bps > 0 {
			pm.bitrateBps = bps
			return pm
		}
	}

	// Fallback: estimate bitrate from size / duration
	if result.Format.Size != "" && pm.durationSec > 0 {
		size, _ := strconv.ParseInt(result.Format.Size, 10, 64)
		if size > 0 {
			pm.bitrateBps = int64(float64(size) * 8 / float64(pm.durationSec))
		}
	}

	return pm
}

func mimeTypeFromExt(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".avi":
		return "video/x-msvideo"
	case ".webm":
		return "video/webm"
	case ".mov":
		return "video/quicktime"
	case ".ts":
		return "video/mp2t"
	case ".flv":
		return "video/x-flv"
	case ".mpg", ".mpeg":
		return "video/mpeg"
	case ".wmv":
		return "video/x-ms-wmv"
	case ".vob":
		return "video/x-ms-vob"
	default:
		return "application/octet-stream"
	}
}

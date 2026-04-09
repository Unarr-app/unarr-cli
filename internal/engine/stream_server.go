package engine

import (
	"context"
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

// StreamServer is a persistent HTTP server that serves one file at a time.
// Start it once with Listen(), then swap files with SetFile()/ClearFile().
// The server stays alive for the entire daemon lifecycle — no port churn.
type StreamServer struct {
	mu       sync.RWMutex
	provider FileProvider
	taskID   string // current task being streamed

	server      *http.Server
	port        int
	url         string     // best single URL (backward compat)
	urls        StreamURLs // all available URLs by network type
	upnpMapping *UPnPMapping
	disableUPnP bool

	lastActivity  atomic.Int64
	maxByteOffset atomic.Int64 // highest sequential read position (main playback connection)
	totalFileSize atomic.Int64
	bitrateBps    atomic.Int64 // video bitrate in bits/sec (from ffprobe, 0 = unknown)
	durationSec   atomic.Int64 // video duration in seconds (from ffprobe, 0 = unknown)
	topReaderID   atomic.Int64 // ID of the reader that set maxByteOffset (only it can advance it)
	readerCounter atomic.Int64 // monotonic counter for assigning reader IDs
}

// NewStreamServer creates a stream server bound to the given port.
// Call Listen() to start accepting connections, then SetFile() to serve content.
func NewStreamServer(port int) *StreamServer {
	return &StreamServer{port: port}
}

// Listen starts the HTTP server on the configured port. Call once at daemon startup.
func (ss *StreamServer) Listen(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/stream", ss.handler)
	mux.HandleFunc("/health", ss.healthHandler)

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
	if !ss.disableUPnP {
		if mapping, err := SetupUPnP(ss.port); err == nil {
			ss.upnpMapping = mapping
			ss.urls.Public = fmt.Sprintf("http://%s:%d/stream", mapping.ExternalIP, mapping.ExternalPort)
		}
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
	return nil
}

// SetFile atomically swaps the file being served and resets progress tracking.
func (ss *StreamServer) SetFile(provider FileProvider, taskID string) {
	ss.mu.Lock()
	ss.provider = provider
	ss.taskID = taskID
	ss.mu.Unlock()
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

// ClearFile stops serving any file. Subsequent requests return 404.
func (ss *StreamServer) ClearFile() {
	ss.mu.Lock()
	ss.provider = nil
	ss.taskID = ""
	ss.mu.Unlock()
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

// HasFile returns true if a file is currently being served.
func (ss *StreamServer) HasFile() bool {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	return ss.provider != nil
}

// URL returns the best single stream URL (backward compat).
func (ss *StreamServer) URL() string { return ss.url }

// URLsJSON returns all available stream URLs as a JSON string.
func (ss *StreamServer) URLsJSON() string {
	b, _ := json.Marshal(ss.urls)
	return string(b)
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
	if ss.server != nil {
		return ss.server.Shutdown(ctx)
	}
	return nil
}

// healthHandler responde con el estado del servidor en JSON.
// Útil para diagnosticar conectividad desde redes remotas o Tailscale:
//
//	curl http://<tailscale-ip>:<port>/health
func (ss *StreamServer) healthHandler(w http.ResponseWriter, r *http.Request) {
	ss.mu.RLock()
	provider := ss.provider
	taskID := ss.taskID
	ss.mu.RUnlock()

	clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)

	type healthResponse struct {
		Status    string `json:"status"`
		Streaming bool   `json:"streaming"`
		File      string `json:"file,omitempty"`
		Task      string `json:"task,omitempty"`
		Port      int    `json:"port"`
		Client    string `json:"client"`
	}
	resp := healthResponse{
		Status: "ok",
		Port:   ss.port,
		Client: clientIP,
	}
	if provider != nil {
		resp.Streaming = true
		resp.File = provider.FileName()
		resp.Task = taskID
		if len(resp.Task) > 8 {
			resp.Task = resp.Task[:8]
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	json.NewEncoder(w).Encode(resp) //nolint:errcheck
}

func (ss *StreamServer) handler(w http.ResponseWriter, r *http.Request) {
	ss.lastActivity.Store(time.Now().UnixNano())

	// Log every incoming request — essential for diagnosing remote/Tailscale issues.
	clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	log.Printf("[stream] %s /stream from %s Range:%q", r.Method, clientIP, r.Header.Get("Range"))

	// Get current provider (may be nil if no file is being served)
	ss.mu.RLock()
	provider := ss.provider
	ss.mu.RUnlock()

	if provider == nil {
		http.Error(w, "no active stream", http.StatusNotFound)
		return
	}

	// CORS headers — only when browser sends Origin (HTTPS site → localhost)
	if origin := r.Header.Get("Origin"); origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Range")
		w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, Accept-Ranges")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
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
func NewTorrentFileProvider(file *torrent.File) FileProvider {
	return &torrentFileProvider{file: file}
}

// torrentFileProvider wraps a torrent.File to implement FileProvider.
type torrentFileProvider struct {
	file *torrent.File
}

func (p *torrentFileProvider) NewFileReader(ctx context.Context) io.ReadSeekCloser {
	reader := p.file.NewReader()
	reader.SetResponsive()
	reader.SetReadahead(5 * 1024 * 1024)
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

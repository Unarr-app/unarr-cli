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
	maxByteOffset atomic.Int64
	totalFileSize atomic.Int64
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

	// SO_REUSEADDR allows immediate rebind if the port is in TIME_WAIT (e.g. after agent restart)
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
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
}

// ClearFile stops serving any file. Subsequent requests return 404.
func (ss *StreamServer) ClearFile() {
	ss.mu.Lock()
	ss.provider = nil
	ss.taskID = ""
	ss.mu.Unlock()
	ss.totalFileSize.Store(0)
	ss.maxByteOffset.Store(0)
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

func (ss *StreamServer) handler(w http.ResponseWriter, r *http.Request) {
	ss.lastActivity.Store(time.Now().UnixNano())

	// Track Range header for watch progress estimation
	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
		if start := parseRangeStart(rangeHeader); start >= 0 {
			for {
				cur := ss.maxByteOffset.Load()
				if start <= cur || ss.maxByteOffset.CompareAndSwap(cur, start) {
					break
				}
			}
		}
	}

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

	reader := provider.NewFileReader(r.Context())
	if reader == nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	defer reader.Close()

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

// EstimatedProgress returns an estimated watch progress based on HTTP Range requests.
func (ss *StreamServer) EstimatedProgress() (position int, duration int) {
	total := ss.totalFileSize.Load()
	if total <= 0 {
		return 0, 0
	}
	maxOffset := ss.maxByteOffset.Load()
	pct := int(float64(maxOffset) / float64(total) * 100)
	if pct > 100 {
		pct = 100
	}
	return pct, 100
}

// parseRangeStart extracts the start byte from a "Range: bytes=START-" header.
func parseRangeStart(rangeHeader string) int64 {
	after, found := strings.CutPrefix(rangeHeader, "bytes=")
	if !found {
		return -1
	}
	dashIdx := strings.IndexByte(after, '-')
	if dashIdx < 0 {
		return -1
	}
	start, err := strconv.ParseInt(after[:dashIdx], 10, 64)
	if err != nil {
		return -1
	}
	return start
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
		log.Printf("stream: failed to open %q: %v", p.path, err)
		return nil
	}
	return f
}

func (p *diskFileProvider) FileName() string { return p.name }

func (p *diskFileProvider) FileSize() int64 {
	fi, err := os.Stat(p.path)
	if err != nil {
		log.Printf("stream: failed to stat %q: %v", p.path, err)
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

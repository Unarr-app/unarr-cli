package engine

import (
	"context"
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
	"sync/atomic"
	"time"

	"github.com/anacrolix/torrent"
)

// fileProvider abstracts where to get a file reader for streaming.
type fileProvider interface {
	NewFileReader(ctx context.Context) io.ReadSeekCloser
	FileName() string
	FileSize() int64
}

// StreamServer serves a torrent file over HTTP with Range request support.
type StreamServer struct {
	provider      fileProvider
	server        *http.Server
	port          int
	url           string
	upnpMapping   *UPnPMapping
	lastActivity  atomic.Int64 // UnixNano of last HTTP request
	maxByteOffset atomic.Int64 // highest byte offset served (for watch progress estimation)
	totalFileSize int64        // total file size in bytes (set on Start)
}

// NewStreamServer creates a new HTTP server for streaming via StreamEngine.
func NewStreamServer(engine *StreamEngine, port int) *StreamServer {
	return &StreamServer{
		provider: engine,
		port:     port,
	}
}

// NewStreamServerFromFile creates a server that streams directly from a torrent.File.
// Used for streaming an active download without a separate StreamEngine.
func NewStreamServerFromFile(file *torrent.File, port int) *StreamServer {
	return &StreamServer{
		provider: &torrentFileProvider{file: file},
		port:     port,
	}
}

// torrentFileProvider wraps a torrent.File to implement fileProvider.
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
		return 0
	}
	return fi.Size()
}

// NewStreamServerFromDisk creates a server that streams a file from disk.
func NewStreamServerFromDisk(filePath string, port int) *StreamServer {
	return &StreamServer{
		provider: &diskFileProvider{
			path: filePath,
			name: filepath.Base(filePath),
		},
		port: port,
	}
}

// FindVideoFile scans a directory (recursively) for the largest video file.
// Returns empty string if no video file found.
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

// Start begins serving the file on all interfaces. Returns the best reachable URL.
// The file is served as-is — the user's media player (VLC, mpv, etc.) handles decoding.
func (ss *StreamServer) Start(ctx context.Context) (string, error) {
	ss.lastActivity.Store(time.Now().UnixNano())
	ss.totalFileSize = ss.provider.FileSize()

	mux := http.NewServeMux()
	mux.HandleFunc("/stream", ss.handler)

	addr := fmt.Sprintf("0.0.0.0:%d", ss.port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("listen on %s: %w", addr, err)
	}

	ss.port = listener.Addr().(*net.TCPAddr).Port
	ss.url = fmt.Sprintf("http://%s:%d/stream", reachableIP(), ss.port)
	log.Printf("stream: serving on %s", ss.url)

	ss.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		if err := ss.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Printf("stream server error: %v", err)
		}
	}()

	return ss.url, nil
}

// URL returns the full stream URL.
func (ss *StreamServer) URL() string { return ss.url }

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

	reader := ss.provider.NewFileReader(r.Context())
	if reader == nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", mimeTypeFromExt(ss.provider.FileName()))

	http.ServeContent(w, r, ss.provider.FileName(), time.Time{}, reader)
}

// EstimatedProgress returns an estimated watch progress based on HTTP Range requests.
// Returns (position, duration) where both are 0-100 scale (percentage-based).
func (ss *StreamServer) EstimatedProgress() (position int, duration int) {
	total := ss.totalFileSize
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
	// Format: "bytes=START-" or "bytes=START-END"
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

// reachableIP returns the best IP to use for the stream URL, in priority order:
//  1. Tailscale IP (100.x.x.x) — accessible from anywhere via Tailscale mesh
//  2. LAN IP — accessible from local network
//  3. 127.0.0.1 — fallback (same machine only)
func reachableIP() string {
	// 1. Try Tailscale — gives an IP reachable from any device in the tailnet
	if ip := tailscaleIP(); ip != "" {
		return ip
	}
	// 2. Fall back to LAN IP
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

// tailscaleIP returns the Tailscale IPv4 address, or "" if Tailscale isn't running.
func tailscaleIP() string {
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

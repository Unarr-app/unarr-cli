package engine

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anacrolix/torrent"
)

// fileProvider abstracts where to get a file reader for streaming.
type fileProvider interface {
	NewFileReader(ctx context.Context) io.ReadSeekCloser
	FileName() string
}

// StreamServer serves a torrent file over HTTP with Range request support.
type StreamServer struct {
	provider fileProvider
	server   *http.Server
	port     int
	url      string
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

// diskFileProvider serves a file from disk.
type diskFileProvider struct {
	path string
	name string
}

func (p *diskFileProvider) NewFileReader(_ context.Context) io.ReadSeekCloser {
	f, err := os.Open(p.path)
	if err != nil {
		return nil
	}
	return f
}

func (p *diskFileProvider) FileName() string { return p.name }

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

// Start begins serving the file on localhost. Returns the full URL.
func (ss *StreamServer) Start(ctx context.Context) (string, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/stream", ss.handler)

	addr := fmt.Sprintf("127.0.0.1:%d", ss.port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("listen on %s: %w", addr, err)
	}

	// Extract actual port (important when port=0)
	ss.port = listener.Addr().(*net.TCPAddr).Port
	ss.url = fmt.Sprintf("http://127.0.0.1:%d/stream", ss.port)

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

// Shutdown gracefully stops the HTTP server.
func (ss *StreamServer) Shutdown(ctx context.Context) error {
	if ss.server != nil {
		return ss.server.Shutdown(ctx)
	}
	return nil
}

func (ss *StreamServer) handler(w http.ResponseWriter, r *http.Request) {
	reader := ss.provider.NewFileReader(r.Context())
	if reader == nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", mimeTypeFromExt(ss.provider.FileName()))

	http.ServeContent(w, r, ss.provider.FileName(), time.Time{}, reader)
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

package engine

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/torrentclaw/unarr/internal/agent"
)

// ---------------------------------------------------------------------------
// StreamEngine unit tests (no network)
// ---------------------------------------------------------------------------

func TestStreamBuildMagnet(t *testing.T) {
	hash := "abc123def456abc123def456abc123def456abc1"
	magnet := buildMagnet(hash)

	if !strings.HasPrefix(magnet, "magnet:?xt=urn:btih:"+hash) {
		t.Errorf("magnet should start with btih, got: %s", magnet[:60])
	}

	// Should contain trackers
	for _, tracker := range defaultTrackers {
		if !strings.Contains(magnet, "tr=") {
			t.Errorf("magnet should contain tracker param for %s", tracker)
		}
	}
}

func TestStreamBuildMagnetPassthrough(t *testing.T) {
	// If input already is a magnet, Start should use it directly
	// Here we test that buildMagnet produces a valid magnet from a hash
	hash := "0000000000000000000000000000000000000000"
	magnet := buildMagnet(hash)
	if !strings.Contains(magnet, hash) {
		t.Error("magnet should contain the info hash")
	}
}

func TestVideoExtensions(t *testing.T) {
	exts := []string{".mkv", ".mp4", ".avi", ".webm", ".mov", ".ts", ".flv", ".m4v", ".mpg", ".mpeg", ".vob", ".wmv"}
	for _, ext := range exts {
		if !VideoExts[ext] {
			t.Errorf("expected %s to be a video extension", ext)
		}
	}

	nonVideo := []string{".txt", ".zip", ".nfo", ".srt", ".jpg", ".exe"}
	for _, ext := range nonVideo {
		if VideoExts[ext] {
			t.Errorf("expected %s to NOT be a video extension", ext)
		}
	}
}

func TestCalculateBufferTarget(t *testing.T) {
	tests := []struct {
		name        string
		totalBytes  int64
		bufferBytes int64
		want        int64
	}{
		{"small file (<200MB) uses 5%", 100 * 1024 * 1024, 0, 100 * 1024 * 1024 / 20},
		{"large file (10GB) caps at 10MB", 10 * 1024 * 1024 * 1024, 0, 10 * 1024 * 1024},
		{"medium file (500MB) caps at 10MB", 500 * 1024 * 1024, 0, 10 * 1024 * 1024}, // 5% of 500MB = 25MB > 10MB cap
		{"override takes precedence", 10 * 1024 * 1024 * 1024, 5 * 1024 * 1024, 5 * 1024 * 1024},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &StreamEngine{
				totalBytes: tt.totalBytes,
				cfg:        StreamConfig{BufferBytes: tt.bufferBytes},
			}
			got := s.calculateBufferTarget()
			if got != tt.want {
				t.Errorf("calculateBufferTarget() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestIsVideoFile(t *testing.T) {
	tests := []struct {
		name     string
		fileName string
		want     bool
	}{
		{"mp4", "movie.mp4", true},
		{"mkv", "movie.mkv", true},
		{"avi", "movie.avi", true},
		{"nfo", "movie.nfo", false},
		{"txt", "readme.txt", false},
		{"srt", "subtitles.srt", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &StreamEngine{fileName: tt.fileName}
			if got := s.IsVideoFile(); got != tt.want {
				t.Errorf("IsVideoFile(%q) = %v, want %v", tt.fileName, got, tt.want)
			}
		})
	}
}

func TestStreamStatusConstants(t *testing.T) {
	// Verify status constants are distinct
	statuses := []StreamStatus{
		StreamStatusMetadata,
		StreamStatusBuffering,
		StreamStatusReady,
		StreamStatusError,
	}
	seen := map[StreamStatus]bool{}
	for _, s := range statuses {
		if seen[s] {
			t.Errorf("duplicate status value: %d", s)
		}
		seen[s] = true
	}
}

func TestStreamEngineGetters(t *testing.T) {
	s := &StreamEngine{
		fileName:     "movie.mkv",
		totalBytes:   4 * 1024 * 1024 * 1024,
		bufferTarget: 10 * 1024 * 1024,
	}

	if s.FileName() != "movie.mkv" {
		t.Errorf("FileName() = %q", s.FileName())
	}
	if s.FileLength() != 4*1024*1024*1024 {
		t.Errorf("FileLength() = %d", s.FileLength())
	}
	if s.BufferTarget() != 10*1024*1024 {
		t.Errorf("BufferTarget() = %d", s.BufferTarget())
	}
}

// ---------------------------------------------------------------------------
// StreamServer unit tests
// ---------------------------------------------------------------------------

func TestMimeTypeFromExt(t *testing.T) {
	tests := []struct {
		filename string
		want     string
	}{
		{"movie.mp4", "video/mp4"},
		{"movie.m4v", "video/mp4"},
		{"movie.mkv", "video/x-matroska"},
		{"movie.avi", "video/x-msvideo"},
		{"movie.webm", "video/webm"},
		{"movie.mov", "video/quicktime"},
		{"movie.ts", "video/mp2t"},
		{"movie.flv", "video/x-flv"},
		{"movie.mpg", "video/mpeg"},
		{"movie.mpeg", "video/mpeg"},
		{"movie.wmv", "video/x-ms-wmv"},
		{"movie.vob", "video/x-ms-vob"},
		{"unknown.xyz", "application/octet-stream"},
		{"file.MP4", "video/mp4"}, // case insensitive
		{"FILE.MKV", "video/x-matroska"},
	}

	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			got := mimeTypeFromExt(tt.filename)
			if got != tt.want {
				t.Errorf("mimeTypeFromExt(%q) = %q, want %q", tt.filename, got, tt.want)
			}
		})
	}
}

func TestStreamServerStartShutdown(t *testing.T) {
	// Test server lifecycle without a real StreamEngine
	// We can't test actual streaming, but we can test the HTTP server mechanics

	// Create a minimal engine with just enough state for the server
	s := &StreamEngine{
		fileName:   "test.mp4",
		totalBytes: 1024,
	}

	srv := NewStreamServer(0)
	if srv.Port() != 0 {
		t.Errorf("initial port should be 0, got %d", srv.Port())
	}

	// Test that Shutdown on an un-started server doesn't panic
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Errorf("shutdown of un-started server should not error: %v", err)
	}

	// Test SetFile/ClearFile
	srv.SetFile(s, "test-task-id")
	if !srv.HasFile() {
		t.Error("HasFile should be true after SetFile")
	}
	if srv.CurrentTaskID() != "test-task-id" {
		t.Errorf("CurrentTaskID = %q, want %q", srv.CurrentTaskID(), "test-task-id")
	}
	srv.ClearFile()
	if srv.HasFile() {
		t.Error("HasFile should be false after ClearFile")
	}
}

// ---------------------------------------------------------------------------
// Task integration with stream fields
// ---------------------------------------------------------------------------

func TestNewTaskFromAgentWithMode(t *testing.T) {
	at := agent.Task{
		ID:              "stream-task-1",
		InfoHash:        "abc123def456abc123def456abc123def456abc1",
		Title:           "Movie (2024)",
		PreferredMethod: "auto",
		Mode:            "stream",
	}
	task := NewTaskFromAgent(at)

	if task.Mode != "stream" {
		t.Errorf("Mode = %q, want stream", task.Mode)
	}
	if task.Status != StatusClaimed {
		t.Errorf("Status = %q, want claimed", task.Status)
	}
}

func TestNewTaskFromAgentDefaultMode(t *testing.T) {
	at := agent.Task{
		ID:              "download-task-1",
		InfoHash:        "abc123def456abc123def456abc123def456abc1",
		PreferredMethod: "auto",
		// Mode not set
	}
	task := NewTaskFromAgent(at)

	if task.Mode != "download" {
		t.Errorf("Mode = %q, want download (default)", task.Mode)
	}
}

func TestToStatusUpdateIncludesStreamURL(t *testing.T) {
	task := &Task{
		ID:              "stream-task-2",
		Status:          StatusDownloading,
		ResolvedMethod:  MethodTorrent,
		Mode:            "stream",
		StreamURL:       "http://127.0.0.1:43210/stream",
		DownloadedBytes: 500,
		TotalBytes:      1000,
		SpeedBps:        100,
		FileName:        "movie.mkv",
	}

	update := task.ToStatusUpdate()
	if update.StreamURL != "http://127.0.0.1:43210/stream" {
		t.Errorf("StreamURL = %q, want http://127.0.0.1:43210/stream", update.StreamURL)
	}
	if update.Status != "downloading" {
		t.Errorf("Status = %q", update.Status)
	}
}

func TestToStatusUpdateNoStreamURL(t *testing.T) {
	task := &Task{
		ID:             "download-task-2",
		Status:         StatusDownloading,
		ResolvedMethod: MethodTorrent,
		Mode:           "download",
	}

	update := task.ToStatusUpdate()
	if update.StreamURL != "" {
		t.Errorf("StreamURL should be empty for download tasks, got %q", update.StreamURL)
	}
}

// ---------------------------------------------------------------------------
// StreamServer HTTP test (with mock ReadSeeker)
// ---------------------------------------------------------------------------

func TestStreamHTTPHandler(t *testing.T) {
	// We create an HTTP handler manually to test Range request support
	// This simulates what StreamServer.handler does, but with a string reader
	content := strings.Repeat("X", 1000) // 1KB of data

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader := strings.NewReader(content)
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeContent(w, r, "test.mp4", time.Time{}, reader)
	})

	// Test full content request
	t.Run("full request", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "/stream", nil)
		rr := &responseRecorder{headers: http.Header{}, body: &strings.Builder{}}
		handler.ServeHTTP(rr, req)

		if rr.statusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", rr.statusCode)
		}
		if ct := rr.headers.Get("Content-Type"); ct != "video/mp4" {
			t.Errorf("Content-Type = %q, want video/mp4", ct)
		}
		if rr.body.Len() != 1000 {
			t.Errorf("body length = %d, want 1000", rr.body.Len())
		}
	})

	// Test Range request
	t.Run("range request", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "/stream", nil)
		req.Header.Set("Range", "bytes=0-99")
		rr := &responseRecorder{headers: http.Header{}, body: &strings.Builder{}}
		handler.ServeHTTP(rr, req)

		if rr.statusCode != http.StatusPartialContent {
			t.Errorf("status = %d, want 206 Partial Content", rr.statusCode)
		}
		if rr.body.Len() != 100 {
			t.Errorf("body length = %d, want 100", rr.body.Len())
		}
	})

	// Test Range request middle
	t.Run("range request middle", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "/stream", nil)
		req.Header.Set("Range", "bytes=500-599")
		rr := &responseRecorder{headers: http.Header{}, body: &strings.Builder{}}
		handler.ServeHTTP(rr, req)

		if rr.statusCode != http.StatusPartialContent {
			t.Errorf("status = %d, want 206", rr.statusCode)
		}
		if rr.body.Len() != 100 {
			t.Errorf("body length = %d, want 100", rr.body.Len())
		}
	})

	// Test HEAD request
	t.Run("HEAD request", func(t *testing.T) {
		req, _ := http.NewRequest("HEAD", "/stream", nil)
		rr := &responseRecorder{headers: http.Header{}, body: &strings.Builder{}}
		handler.ServeHTTP(rr, req)

		if rr.statusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", rr.statusCode)
		}
	})
}

// responseRecorder is a minimal http.ResponseWriter for testing
type responseRecorder struct {
	statusCode int
	headers    http.Header
	body       *strings.Builder
}

func (r *responseRecorder) Header() http.Header  { return r.headers }
func (r *responseRecorder) WriteHeader(code int) { r.statusCode = code }
func (r *responseRecorder) Write(b []byte) (int, error) {
	if r.statusCode == 0 {
		r.statusCode = http.StatusOK
	}
	return r.body.Write(b)
}

// Ensure responseRecorder implements ReadSeeker expectations
func (r *responseRecorder) ReadFrom(src io.Reader) (int64, error) {
	n, err := io.Copy(r.body, src)
	return n, err
}

// TestPrioritizeTail_SmallFile verifica que PrioritizeTail no lanza goroutine
// cuando el archivo es demasiado pequeño (≤ 2×tailBytes).
func TestPrioritizeTail_SmallFile(t *testing.T) {
	s := &StreamEngine{
		totalBytes: 5 * 1024 * 1024, // 5 MB — menor que 2×5 MB
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// No debe entrar en pánico ni bloquear con file == nil
	s.PrioritizeTail(ctx, 5*1024*1024)
	// Si llega aquí sin pánico, el test pasa
}

// TestPrioritizeTail_NilFile verifica que PrioritizeTail es seguro cuando
// file es nil (engine no inicializado).
func TestPrioritizeTail_NilFile(t *testing.T) {
	s := &StreamEngine{
		totalBytes: 100 * 1024 * 1024,
		file:       nil,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.PrioritizeTail(ctx, 5*1024*1024)
	// No debe entrar en pánico
}

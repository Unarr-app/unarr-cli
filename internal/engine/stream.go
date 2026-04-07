package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	alog "github.com/anacrolix/log"
	"github.com/anacrolix/torrent"
)

// StreamConfig holds settings for the streaming engine.
type StreamConfig struct {
	DataDir     string
	Port        int
	BufferBytes int64
	MetaTimeout time.Duration
	NoOpen      bool
	PlayerCmd   string
}

// StreamStatus represents the current state of the streaming session.
type StreamStatus int

const (
	StreamStatusMetadata StreamStatus = iota
	StreamStatusBuffering
	StreamStatusReady
	StreamStatusError
)

// StreamProgress is a snapshot of current streaming stats.
type StreamProgress struct {
	Status          StreamStatus
	DownloadedBytes int64
	TotalBytes      int64
	SpeedBps        int64
	Peers           int
	Seeds           int
	FileName        string
}

// StreamEngine manages a single streaming torrent session.
type StreamEngine struct {
	client *torrent.Client
	cfg    StreamConfig
	tor    *torrent.Torrent
	file   *torrent.File

	bufferTarget int64
	totalBytes   int64
	fileName     string

	mu        sync.RWMutex
	status    StreamStatus
	lastBytes int64
	lastTime  time.Time
	speedBps  int64
}

// NewStreamEngine creates a streaming engine with its own torrent client.
func NewStreamEngine(cfg StreamConfig) (*StreamEngine, error) {
	if cfg.MetaTimeout == 0 {
		cfg.MetaTimeout = 60 * time.Second
	}

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	tcfg := torrent.NewDefaultClientConfig()
	tcfg.DataDir = cfg.DataDir
	tcfg.Seed = false
	tcfg.NoUpload = true
	tcfg.ListenPort = 0
	tcfg.Logger = alog.Default.FilterLevel(alog.Disabled)

	client, err := torrent.NewClient(tcfg)
	if err != nil {
		return nil, fmt.Errorf("create torrent client: %w", err)
	}

	return &StreamEngine{
		client: client,
		cfg:    cfg,
		status: StreamStatusMetadata,
	}, nil
}

// Start adds the torrent, waits for metadata, selects the video file,
// and prepares for streaming.
func (s *StreamEngine) Start(ctx context.Context, magnetOrHash string) error {
	magnet := magnetOrHash
	if !strings.HasPrefix(magnet, "magnet:") {
		magnet = buildMagnet(strings.TrimSpace(magnetOrHash))
	}

	t, err := s.client.AddMagnet(magnet)
	if err != nil {
		return fmt.Errorf("add magnet: %w", err)
	}
	s.tor = t

	metaCtx, metaCancel := context.WithTimeout(ctx, s.cfg.MetaTimeout)
	defer metaCancel()

	select {
	case <-t.GotInfo():
	case <-metaCtx.Done():
		return fmt.Errorf("metadata timeout after %s: no peers found", s.cfg.MetaTimeout)
	}

	if err := s.selectFile(); err != nil {
		return err
	}

	s.totalBytes = s.file.Length()
	s.fileName = filepath.Base(s.file.DisplayPath())
	s.bufferTarget = s.calculateBufferTarget()
	s.lastTime = time.Now()

	s.mu.Lock()
	s.status = StreamStatusBuffering
	s.mu.Unlock()

	return nil
}

// selectFile picks the best video file from the torrent.
// Falls back to the largest file if no video is found.
func (s *StreamEngine) selectFile() error {
	files := s.tor.Files()
	if len(files) == 0 {
		return fmt.Errorf("torrent has no files")
	}

	var bestVideo *torrent.File
	var bestAny *torrent.File

	for _, f := range files {
		ext := strings.ToLower(filepath.Ext(f.DisplayPath()))
		if VideoExts[ext] {
			if bestVideo == nil || f.Length() > bestVideo.Length() {
				bestVideo = f
			}
		}
		if bestAny == nil || f.Length() > bestAny.Length() {
			bestAny = f
		}
	}

	if bestVideo != nil {
		s.file = bestVideo
	} else {
		s.file = bestAny
	}

	// Cancel all other files, download only the selected one
	for _, f := range files {
		if f == s.file {
			f.Download()
		} else {
			f.SetPriority(torrent.PiecePriorityNone)
		}
	}

	return nil
}

// IsVideoFile returns true if the selected file has a video extension.
func (s *StreamEngine) IsVideoFile() bool {
	ext := strings.ToLower(filepath.Ext(s.fileName))
	return VideoExts[ext]
}

func (s *StreamEngine) calculateBufferTarget() int64 {
	if s.cfg.BufferBytes > 0 {
		return s.cfg.BufferBytes
	}
	fivePercent := s.totalBytes / 20
	tenMB := int64(10 * 1024 * 1024)
	if fivePercent < tenMB {
		return fivePercent
	}
	return tenMB
}

// contiguousBytes returns the number of bytes completed contiguously
// from the start of the file.
func (s *StreamEngine) contiguousBytes() int64 {
	states := s.file.State()
	var total int64
	for _, ps := range states {
		if ps.Complete {
			total += ps.Bytes
		} else {
			break
		}
	}
	return total
}

// WaitBuffer blocks until enough contiguous bytes from the file start
// are downloaded, or the context is cancelled.
func (s *StreamEngine) WaitBuffer(ctx context.Context, progressFn func(buffered, target int64)) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			buffered := s.contiguousBytes()
			if progressFn != nil {
				progressFn(buffered, s.bufferTarget)
			}
			if buffered >= s.bufferTarget {
				s.mu.Lock()
				s.status = StreamStatusReady
				s.mu.Unlock()
				return nil
			}
		}
	}
}

// NewFileReader creates a new reader for the selected file.
// Each HTTP request should get its own reader (not safe for concurrent use).
func (s *StreamEngine) NewFileReader(ctx context.Context) io.ReadSeekCloser {
	reader := s.file.NewReader()
	reader.SetResponsive()
	reader.SetReadahead(5 * 1024 * 1024) // 5MB readahead
	reader.SetContext(ctx)
	return reader
}

// StartProgressLoop starts a goroutine that updates speed/peer stats every second.
// It stops when the context is cancelled.
func (s *StreamEngine) StartProgressLoop(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				now := time.Now()
				downloaded := s.file.BytesCompleted()

				s.mu.Lock()
				elapsed := now.Sub(s.lastTime).Seconds()
				if elapsed > 0 {
					s.speedBps = int64(float64(downloaded-s.lastBytes) / elapsed)
					if s.speedBps < 0 {
						s.speedBps = 0
					}
				}
				s.lastBytes = downloaded
				s.lastTime = now
				s.mu.Unlock()
			}
		}
	}()
}

// Progress returns a snapshot of the current streaming stats.
func (s *StreamEngine) Progress() StreamProgress {
	s.mu.RLock()
	status := s.status
	speed := s.speedBps
	s.mu.RUnlock()

	stats := s.tor.Stats()

	return StreamProgress{
		Status:          status,
		DownloadedBytes: s.file.BytesCompleted(),
		TotalBytes:      s.totalBytes,
		SpeedBps:        speed,
		Peers:           stats.ActivePeers,
		Seeds:           stats.ConnectedSeeders,
		FileName:        s.fileName,
	}
}

// FileName returns the name of the selected file.
func (s *StreamEngine) FileName() string { return s.fileName }

// FileLength returns the total size of the selected file in bytes.
func (s *StreamEngine) FileLength() int64 { return s.totalBytes }

// FileSize implements FileProvider for StreamServer compatibility.
func (s *StreamEngine) FileSize() int64 { return s.totalBytes }

// BufferTarget returns the buffer threshold in bytes.
func (s *StreamEngine) BufferTarget() int64 { return s.bufferTarget }

// Shutdown gracefully closes the torrent and client.
func (s *StreamEngine) Shutdown(_ context.Context) error {
	if s.tor != nil {
		s.tor.Drop()
	}
	if s.client != nil {
		errs := s.client.Close()
		if len(errs) > 0 {
			return fmt.Errorf("close client: %v", errs[0])
		}
	}
	return nil
}

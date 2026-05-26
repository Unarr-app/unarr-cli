// Package engine — hls.go implements the HLS streaming pipeline.
//
// Browser ↔ daemon over plain HTTP (LAN / Tailscale / UPnP). The daemon runs
// ffmpeg in `-f hls` mode, writing fragmented MP4 segments to a per-session
// tmpdir. Master + media playlists are pre-rendered from the probed source
// duration so the player knows the full timeline before any segment exists.
//
// One HLSSession == one browser playback. Sessions are registered in a
// process-wide map keyed by session ID; the StreamServer routes
//   GET /hls/<id>/master.m3u8
//   GET /hls/<id>/video/index.m3u8
//   GET /hls/<id>/video/init.mp4
//   GET /hls/<id>/video/seg-<n>.m4s
//   GET /hls/<id>/subs/<lang>.vtt
// to the matching session.

package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// hlsSegmentDuration is the target seconds per HLS fragment. Four seconds is
// the Plex/Apple default — short enough that seek granularity is acceptable,
// long enough that GOP overhead doesn't dominate.
const hlsSegmentDuration = 4

// hlsSessionTTL is how long a session can sit idle (no segment requests)
// before the manager kills ffmpeg + cleans the tmpdir.
const hlsSessionTTL = 30 * time.Minute

// hlsTmpDirRoot returns the per-user tmpdir root for HLS sessions.
//
//	Linux:   ~/.cache/unarr/hls-sessions
//	macOS:   ~/Library/Caches/unarr/hls-sessions
//	Windows: %LOCALAPPDATA%/unarr/hls-sessions
//
// Falls back to os.TempDir() if the user cache dir can't be resolved.
func hlsTmpDirRoot() string {
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "unarr", "hls-sessions")
	}
	return filepath.Join(os.TempDir(), "unarr-hls-sessions")
}

// CleanupHLSOrphanDirs removes any per-session tmpdir under hlsTmpDirRoot
// that's older than 1 h. Daemon restart drops the in-memory session
// registry but leaves tmpdirs behind; on the next start we GC them so
// disk usage doesn't grow unbounded across restarts. Sessions started
// less than 1 h ago might still belong to the daemon we're booting (race
// during a quick restart) — leave those alone.
func CleanupHLSOrphanDirs() error {
	root := hlsTmpDirRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	cutoff := time.Now().Add(-1 * time.Hour)
	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.RemoveAll(filepath.Join(root, e.Name())); err == nil {
				removed++
			}
		}
	}
	if removed > 0 {
		log.Printf("[hls] cleaned %d orphan tmpdir(s) at startup", removed)
	}
	return nil
}

// HLSSessionConfig describes a single browser playback session driven by HLS.
type HLSSessionConfig struct {
	SessionID  string
	SourcePath string
	FileName   string
	Quality    string // "2160p"|"1080p"|"720p"|"480p"|"original"|""
	AudioIndex int    // 0-based ffmpeg audio stream selection (-map 0:a:N). -1 = default.
	Transcode  TranscodeRuntime
}

// HLSSession owns a tmpdir + ffmpeg subprocess producing HLS fragments.
//
// Seek behaviour: ffmpeg writes segments sequentially from `ffmpegSegStart`.
// When a handler asks for a segment far ahead of the writer, the daemon
// kills the current ffmpeg and restarts it with `-ss <targetSec>
// -output_ts_offset <targetSec> -start_number <idx>` so the next segments
// it emits land at the requested timeline position. Segments already on
// disk before the seek stay there; the new ffmpeg only writes from the
// target index forward.
type HLSSession struct {
	cfg   HLSSessionConfig
	probe *StreamProbe

	tmpDir        string
	durationSec   float64
	segmentCount  int
	manifestVideo string // pre-rendered video media playlist
	manifestRoot  string // pre-rendered master playlist

	mu             sync.Mutex
	cmd            *exec.Cmd
	cancel         context.CancelFunc
	closed         bool
	startedAt      time.Time
	lastTouch      time.Time
	ffmpegSegStart int // index of the first segment the current ffmpeg writes
	restartCount   int // bounded auto-restart counter (resets on Close)
	lastRestartAt  time.Time

	// readyCond + readyMax track which segments ffmpeg has finished writing.
	// Handlers waiting on a future segment block on readyCond until the
	// poller advances readyMax past their index (or ffmpeg exits).
	readyMu  sync.Mutex
	readyMax int // highest segment index whose .m4s file is fully written
	exitErr  error
	exited   bool
	readyCh  chan struct{} // closed + replaced each time readyMax advances
}

// hlsSeekAhead is how many segments past the writer's current position the
// browser is allowed to request before we restart ffmpeg from the requested
// segment. 8 segments * 4 s = 32 s of "warm" buffer; further seeks trigger
// a restart instead of waiting through real-time encode.
const hlsSeekAhead = 8

// HLSSessionRegistry tracks active sessions keyed by ID.
type HLSSessionRegistry struct {
	mu       sync.RWMutex
	sessions map[string]*HLSSession
}

// NewHLSSessionRegistry returns an empty registry.
func NewHLSSessionRegistry() *HLSSessionRegistry {
	return &HLSSessionRegistry{sessions: make(map[string]*HLSSession)}
}

// Get fetches a session by ID; returns nil if not registered.
func (r *HLSSessionRegistry) Get(id string) *HLSSession {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sessions[id]
}

// Register adds a session under its ID. Replaces any previous session with
// the same ID (which is closed first to release ffmpeg + tmpdir).
//
// Also closes EVERY OTHER active session, since one daemon == one viewer ==
// one stream at a time. Without this, repeatedly opening the player (or
// changing quality) leaves orphan ffmpegs running until the 30 min idle
// sweeper reaps them, and N concurrent transcodes saturate the CPU.
func (r *HLSSessionRegistry) Register(s *HLSSession) {
	r.mu.Lock()
	stale := make([]*HLSSession, 0, len(r.sessions))
	for id, prev := range r.sessions {
		if id == s.cfg.SessionID {
			stale = append(stale, prev)
			continue
		}
		stale = append(stale, prev)
		delete(r.sessions, id)
	}
	r.sessions[s.cfg.SessionID] = s
	r.mu.Unlock()
	for _, prev := range stale {
		_ = prev.Close()
	}
}

// Remove drops a session from the registry without closing it.
func (r *HLSSessionRegistry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, id)
}

// CloseAll terminates every active session. Call at daemon shutdown.
func (r *HLSSessionRegistry) CloseAll() {
	r.mu.Lock()
	sessions := make([]*HLSSession, 0, len(r.sessions))
	for _, s := range r.sessions {
		sessions = append(sessions, s)
	}
	r.sessions = make(map[string]*HLSSession)
	r.mu.Unlock()
	for _, s := range sessions {
		_ = s.Close()
	}
}

// SweepIdle closes sessions that have not been touched within hlsSessionTTL.
// Returns the number of sessions reaped.
func (r *HLSSessionRegistry) SweepIdle() int {
	r.mu.Lock()
	stale := make([]*HLSSession, 0)
	for id, s := range r.sessions {
		s.mu.Lock()
		idle := time.Since(s.lastTouch)
		s.mu.Unlock()
		if idle > hlsSessionTTL {
			stale = append(stale, s)
			delete(r.sessions, id)
		}
	}
	r.mu.Unlock()
	for _, s := range stale {
		_ = s.Close()
	}
	return len(stale)
}

// StartHLSSession probes the source, builds the playlists, spawns ffmpeg,
// and returns a HLSSession ready to serve HTTP requests. Caller must register
// the session with a HLSSessionRegistry so the server can route to it.
func StartHLSSession(ctx context.Context, cfg HLSSessionConfig) (*HLSSession, error) {
	if cfg.SessionID == "" {
		return nil, errors.New("hls: empty session id")
	}
	if !validSessionID.MatchString(cfg.SessionID) {
		return nil, errors.New("hls: invalid session id")
	}
	if cfg.SourcePath == "" {
		return nil, errors.New("hls: empty source path")
	}
	if cfg.Transcode.FFmpegPath == "" || cfg.Transcode.FFprobePath == "" {
		return nil, errors.New("hls: ffmpeg/ffprobe not available")
	}

	// Probe gets a 15 s ceiling. ffprobe on a 50 GB MKV over a slow remote
	// fs can hang indefinitely; without a deadline the daemon would block
	// the goroutine that started the session forever and the user would
	// see the player phase stuck on "Preparando sesión".
	probeCtx, cancelProbe := context.WithTimeout(ctx, 15*time.Second)
	probe, err := ProbeFile(probeCtx, cfg.Transcode.FFprobePath, cfg.SourcePath)
	cancelProbe()
	if err != nil {
		return nil, fmt.Errorf("hls: probe: %w", err)
	}
	if probe.DurationSec <= 0 {
		return nil, errors.New("hls: source has no duration")
	}

	tmpDir := filepath.Join(hlsTmpDirRoot(), cfg.SessionID)
	if err := os.MkdirAll(filepath.Join(tmpDir, "video"), 0o755); err != nil {
		return nil, fmt.Errorf("hls: mkdir video: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "subs"), 0o755); err != nil {
		return nil, fmt.Errorf("hls: mkdir subs: %w", err)
	}

	segCount := int((probe.DurationSec + float64(hlsSegmentDuration) - 1) / float64(hlsSegmentDuration))
	if segCount < 1 {
		segCount = 1
	}

	s := &HLSSession{
		cfg:          cfg,
		probe:        probe,
		tmpDir:       tmpDir,
		durationSec:  probe.DurationSec,
		segmentCount: segCount,
		startedAt:    time.Now(),
		lastTouch:    time.Now(),
		readyCh:      make(chan struct{}),
	}
	s.manifestVideo = renderVideoPlaylist(probe.DurationSec, segCount)
	s.manifestRoot = renderMasterPlaylist(probe, cfg.Quality)

	// Spawn ffmpeg under a dedicated context so Close() can kill it without
	// touching the parent ctx.
	ffCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	args := buildHLSFFmpegArgs(cfg, probe, tmpDir)
	cmd := exec.CommandContext(ffCtx, cfg.Transcode.FFmpegPath, args...)
	cmd.Stderr = &hlsStderrCapture{owner: s}
	if err := cmd.Start(); err != nil {
		cancel()
		_ = os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("hls: start ffmpeg: %w", err)
	}
	s.cmd = cmd

	go s.waitFFmpeg()
	go s.pollSegments(ffCtx)

	if len(probe.SubtitleTracks) > 0 {
		go s.extractSubtitles(ffCtx)
	}

	log.Printf("[hls %s] started: %s, %.1fs, %d segs (quality=%s)",
		shortHLSID(cfg.SessionID), filepath.Base(cfg.SourcePath),
		probe.DurationSec, segCount, coalesce(cfg.Quality, "auto"))
	return s, nil
}

// shortHLSID truncates a session ID for log lines.
func shortHLSID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// ProbeInfo returns a JSON-friendly summary of the source media so the web
// player can render quality / codec / track info without re-probing.
func (s *HLSSession) ProbeInfo() map[string]any {
	if s.probe == nil {
		return map[string]any{}
	}
	audios := make([]map[string]any, 0, len(s.probe.AudioTracks))
	for _, a := range s.probe.AudioTracks {
		audios = append(audios, map[string]any{
			"index":    a.Index,
			"lang":     a.Lang,
			"codec":    a.Codec,
			"channels": a.Channels,
			"title":    a.Title,
			"default":  a.Default,
		})
	}
	subs := make([]map[string]any, 0, len(s.probe.SubtitleTracks))
	for _, sb := range s.probe.SubtitleTracks {
		subs = append(subs, map[string]any{
			"index":  sb.Index,
			"lang":   sb.Lang,
			"codec":  sb.Codec,
			"title":  sb.Title,
			"forced": sb.Forced,
			"text":   sb.IsTextSubtitle(),
		})
	}
	return map[string]any{
		"videoCodec":  s.probe.VideoCodec,
		"width":       s.probe.Width,
		"height":      s.probe.Height,
		"bitDepth":    s.probe.BitDepth,
		"hdr":         s.probe.HDR,
		"durationSec": s.probe.DurationSec,
		"container":   s.probe.Container,
		"audio":       audios,
		"subtitles":   subs,
	}
}

// MasterPlaylist returns the rendered master.m3u8 contents.
func (s *HLSSession) MasterPlaylist() string { return s.manifestRoot }

// VideoPlaylist returns the rendered video media playlist contents.
func (s *HLSSession) VideoPlaylist() string { return s.manifestVideo }

// DurationSeconds returns the source duration in seconds.
func (s *HLSSession) DurationSeconds() float64 { return s.durationSec }

// Probe returns the probe metadata used to start the session.
func (s *HLSSession) Probe() *StreamProbe { return s.probe }

// Touch updates the last-activity timestamp; the registry sweeper compares
// this against hlsSessionTTL.
func (s *HLSSession) Touch() {
	s.mu.Lock()
	s.lastTouch = time.Now()
	s.mu.Unlock()
}

// Close stops ffmpeg, deletes the tmpdir, and prevents further requests from
// blocking on segment readiness. Idempotent.
func (s *HLSSession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	cancel := s.cancel
	tmpDir := s.tmpDir
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	// Unblock any handler waiting on readyCh.
	s.readyMu.Lock()
	if s.readyCh != nil {
		close(s.readyCh)
		s.readyCh = nil
	}
	s.exited = true
	s.readyMu.Unlock()
	if tmpDir != "" {
		_ = os.RemoveAll(tmpDir)
	}
	log.Printf("[hls %s] closed", shortHLSID(s.cfg.SessionID))
	return nil
}

// waitFFmpeg reaps the ffmpeg process and records its exit error for handlers.
//
// Auto-restart supervisor: if ffmpeg crashes (non-graceful exit) and the
// session is still in use, we attempt to restart it from the last known
// good segment. Bounded to maxRestarts within restartWindow to avoid
// thrashing on a permanently broken source.
func (s *HLSSession) waitFFmpeg() {
	err := s.cmd.Wait()
	s.readyMu.Lock()
	s.exitErr = err
	s.exited = true
	if s.readyCh != nil {
		close(s.readyCh)
		s.readyCh = nil
	}
	readyMax := s.readyMax
	s.readyMu.Unlock()

	if err == nil || s.isClosed() {
		return
	}
	log.Printf("[hls %s] ffmpeg exited: %v", shortHLSID(s.cfg.SessionID), err)

	// Decide whether to attempt an auto-restart. We don't restart when:
	//   - the session was closed externally (kill on quality change etc.)
	//   - we've already retried 3 times within the last 60 s (broken file)
	const maxRestarts = 3
	const restartWindow = 60 * time.Second
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	// Reset the counter when the previous restart was outside the window;
	// the IsZero check is unnecessary because zero time is well in the past
	// and would also satisfy the "outside window" branch.
	if time.Since(s.lastRestartAt) > restartWindow {
		s.restartCount = 0
	}
	if s.restartCount >= maxRestarts {
		s.mu.Unlock()
		log.Printf("[hls %s] giving up after %d auto-restarts", shortHLSID(s.cfg.SessionID), maxRestarts)
		return
	}
	s.restartCount++
	s.lastRestartAt = time.Now()
	s.mu.Unlock()

	// Restart from the last segment we know is safely on disk. If readyMax
	// is 0 (never produced anything), retry from segment 0 — covers initial
	// startup failures on transient errors.
	target := readyMax
	if target < 0 {
		target = 0
	}
	log.Printf("[hls %s] auto-restarting from segment %d (attempt %d/%d)",
		shortHLSID(s.cfg.SessionID), target, s.restartCount, maxRestarts)
	if rerr := s.restartFromSegment(target); rerr != nil {
		log.Printf("[hls %s] auto-restart failed: %v", shortHLSID(s.cfg.SessionID), rerr)
	}
}

// pollSegments watches the video tmpdir for newly-finished .m4s files and
// advances readyMax. ffmpeg writes a segment by first creating an empty
// file, then closing+renaming on completion (atomic-replace), so we use
// stat size > 0 + presence of the *next* segment as proof the previous one
// is done. For the last segment, ffmpeg's exit terminates the wait.
func (s *HLSSession) pollSegments(ctx context.Context) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	videoDir := filepath.Join(s.tmpDir, "video")
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		// Walk segment files and find the highest contiguous index whose
		// successor exists (which proves the segment is fully closed).
		s.readyMu.Lock()
		start := s.readyMax
		exited := s.exited
		s.readyMu.Unlock()

		highest := start
		for i := start; i < s.segmentCount; i++ {
			cur := filepath.Join(videoDir, fmt.Sprintf("seg-%d.m4s", i))
			next := filepath.Join(videoDir, fmt.Sprintf("seg-%d.m4s", i+1))
			ci, err := os.Stat(cur)
			if err != nil || ci.Size() == 0 {
				break
			}
			// Last segment is "ready" only when ffmpeg has exited (no successor
			// can ever appear) or when a later segment exists.
			if i == s.segmentCount-1 {
				if !exited {
					break
				}
				highest = i + 1
				break
			}
			if _, err := os.Stat(next); err != nil {
				break
			}
			highest = i + 1
		}
		if highest > start {
			s.readyMu.Lock()
			s.readyMax = highest
			ch := s.readyCh
			s.readyCh = make(chan struct{})
			s.readyMu.Unlock()
			if ch != nil {
				close(ch)
			}
		}
		if exited && highest >= s.segmentCount {
			return
		}
	}
}

// waitForSegment blocks until segment idx has been fully written, ffmpeg
// has exited, or ctx is cancelled. Returns nil iff the segment file is
// safe to read at return time.
func (s *HLSSession) waitForSegment(ctx context.Context, idx int) error {
	deadline := time.Now().Add(60 * time.Second)
	for {
		s.readyMu.Lock()
		ready := idx < s.readyMax
		exited := s.exited
		ch := s.readyCh
		exitErr := s.exitErr
		s.readyMu.Unlock()
		if ready {
			return nil
		}
		if exited {
			if exitErr != nil {
				return fmt.Errorf("hls: ffmpeg exited: %w", exitErr)
			}
			return errors.New("hls: ffmpeg exited before segment ready")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
			// loop and re-check
		case <-time.After(time.Until(deadline)):
			return errors.New("hls: timeout waiting for segment")
		}
		if time.Now().After(deadline) {
			return errors.New("hls: timeout waiting for segment")
		}
	}
}

// isClosed reports whether Close() has been invoked.
func (s *HLSSession) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// ---- HTTP handlers ----

// ServeMaster writes master.m3u8 to w.
func (s *HLSSession) ServeMaster(w http.ResponseWriter, r *http.Request) {
	s.Touch()
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = io.WriteString(w, s.manifestRoot)
}

// ServeVideoPlaylist writes the video media playlist (index.m3u8) to w.
func (s *HLSSession) ServeVideoPlaylist(w http.ResponseWriter, r *http.Request) {
	s.Touch()
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = io.WriteString(w, s.manifestVideo)
}

// ServeInit writes init.mp4 (the fMP4 init segment) to w.
func (s *HLSSession) ServeInit(w http.ResponseWriter, r *http.Request) {
	s.Touch()
	path := filepath.Join(s.tmpDir, "video", "init.mp4")
	// Init segment is the first thing ffmpeg writes — wait briefly for it.
	deadline := time.Now().Add(30 * time.Second)
	for {
		if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
			break
		}
		if s.isClosed() || time.Now().After(deadline) {
			http.Error(w, "init segment unavailable", http.StatusServiceUnavailable)
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "max-age=3600")
	http.ServeFile(w, r, path)
}

// ServeSegment writes the requested video segment, blocking until ffmpeg
// produces it (capped by waitForSegment timeout).
//
// Seek-restart: if the requested segment is far ahead of where the current
// ffmpeg writer is producing AND it's not already on disk, we kill ffmpeg
// and restart it from the requested position. Without this, a user dragging
// the scrubber to minute 30 would block until the encoder reaches minute 30
// in real time (~25 minutes wait at 1080p software encode).
func (s *HLSSession) ServeSegment(w http.ResponseWriter, r *http.Request, idx int) {
	s.Touch()
	if idx < 0 || idx >= s.segmentCount {
		http.Error(w, "segment out of range", http.StatusNotFound)
		return
	}

	path := filepath.Join(s.tmpDir, "video", fmt.Sprintf("seg-%d.m4s", idx))
	// Fast path: file already on disk (either current writer reached it, or
	// a previous session left it there before a seek-restart).
	if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Cache-Control", "max-age=3600")
		http.ServeFile(w, r, path)
		return
	}

	// Decide if we should restart ffmpeg from the requested segment. Check
	// segStart vs idx — if the gap is wider than hlsSeekAhead and the file
	// isn't on disk, the writer would take too long to reach it.
	s.mu.Lock()
	segStart := s.ffmpegSegStart
	s.mu.Unlock()
	s.readyMu.Lock()
	readyMax := s.readyMax
	s.readyMu.Unlock()

	if idx >= readyMax+hlsSeekAhead || idx < segStart {
		if err := s.restartFromSegment(idx); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
	}

	if err := s.waitForSegment(r.Context(), idx); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "max-age=3600")
	http.ServeFile(w, r, path)
}

// restartFromSegment kills the current ffmpeg, then spawns a new one whose
// `-ss` offset corresponds to segment `targetIdx`. The caller must NOT hold
// s.mu when calling — the function takes both s.mu and s.readyMu.
func (s *HLSSession) restartFromSegment(targetIdx int) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("hls: session closed")
	}
	// `s.exited` lives under s.readyMu (see field comment near declaration);
	// take that lock briefly so the read-modify-write composite check below
	// is consistent with `pollSegments` / `waitFFmpeg` writers.
	s.readyMu.Lock()
	exited := s.exited
	s.readyMu.Unlock()
	if targetIdx == s.ffmpegSegStart && !exited {
		// Already writing from this point — nothing to do.
		s.mu.Unlock()
		return nil
	}
	prevCancel := s.cancel
	prevCmd := s.cmd
	s.mu.Unlock()

	if prevCancel != nil {
		prevCancel()
	}
	if prevCmd != nil && prevCmd.Process != nil {
		_ = prevCmd.Process.Kill()
	}
	// Wait for old ffmpeg to exit so its file handles release. waitFFmpeg
	// (the original goroutine) sets s.exited = true; poll until it does.
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.readyMu.Lock()
		exited := s.exited
		s.readyMu.Unlock()
		if exited {
			break
		}
		if time.Now().After(deadline) {
			break // proceed anyway; new ffmpeg will overwrite
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Build args for the new ffmpeg with -ss offset.
	startSec := float64(targetIdx * hlsSegmentDuration)
	args := buildHLSFFmpegArgsAt(s.cfg, s.probe, s.tmpDir, targetIdx, startSec)

	ffCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ffCtx, s.cfg.Transcode.FFmpegPath, args...)
	cmd.Stderr = &hlsStderrCapture{owner: s}
	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("hls: restart ffmpeg: %w", err)
	}

	// Reset session state so the poll + wait machinery picks up the new run.
	s.mu.Lock()
	s.cmd = cmd
	s.cancel = cancel
	s.ffmpegSegStart = targetIdx
	s.mu.Unlock()

	s.readyMu.Lock()
	s.readyMax = targetIdx // new writer's segments start at targetIdx
	s.exited = false
	s.exitErr = nil
	s.readyCh = make(chan struct{})
	s.readyMu.Unlock()

	go s.waitFFmpeg()
	go s.pollSegments(ffCtx)

	log.Printf("[hls %s] restarted ffmpeg at segment %d (%.1fs)",
		shortHLSID(s.cfg.SessionID), targetIdx, startSec)
	return nil
}

// ServeSubtitle writes the WebVTT subtitle for the requested track index, if
// extraction has finished.
func (s *HLSSession) ServeSubtitle(w http.ResponseWriter, r *http.Request, idx int) {
	s.Touch()
	if idx < 0 || idx >= len(s.probe.SubtitleTracks) {
		http.Error(w, "subtitle track not found", http.StatusNotFound)
		return
	}
	path := filepath.Join(s.tmpDir, "subs", fmt.Sprintf("sub-%d.vtt", idx))
	deadline := time.Now().Add(15 * time.Second)
	for {
		if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
			break
		}
		if s.isClosed() || time.Now().After(deadline) {
			http.Error(w, "subtitle not yet extracted", http.StatusServiceUnavailable)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	w.Header().Set("Cache-Control", "max-age=3600")
	http.ServeFile(w, r, path)
}

// ---- ffmpeg argument builders ----

// buildHLSFFmpegArgs returns the argv for the initial HLS encode (start at 0).
func buildHLSFFmpegArgs(cfg HLSSessionConfig, probe *StreamProbe, tmpDir string) []string {
	return buildHLSFFmpegArgsAt(cfg, probe, tmpDir, 0, 0)
}

// buildHLSFFmpegArgsAt returns the argv for an HLS encode that starts at the
// given segment index (`-ss <startSec>`) and writes segments numbered from
// startIdx so they slot into the existing manifest at the correct position.
// `-output_ts_offset` keeps the segment PTS aligned with manifest timeline.
func buildHLSFFmpegArgsAt(cfg HLSSessionConfig, probe *StreamProbe, tmpDir string, startIdx int, startSec float64) []string {
	hwHint := cfg.Transcode.HWAccel
	args := []string{"-y", "-hide_banner", "-loglevel", "warning"}

	switch hwHint {
	case HWAccelNVENC:
		args = append(args, "-hwaccel", "cuda")
	case HWAccelQSV:
		args = append(args, "-hwaccel", "qsv")
	case HWAccelVAAPI:
		args = append(args, "-hwaccel", "vaapi", "-hwaccel_output_format", "vaapi")
	case HWAccelNone, HWAccelVideoToolbox:
		// No demuxer-side hint.
	}

	// Seek before -i for fast keyframe-aligned start. The new ffmpeg writes
	// segments with PTS shifted via -output_ts_offset so the manifest's
	// pre-computed segment numbering still matches the timeline.
	if startSec > 0 {
		args = append(args, "-ss", strconv.FormatFloat(startSec, 'f', 3, 64))
	}

	args = append(args, "-i", cfg.SourcePath)

	if startSec > 0 {
		args = append(args, "-output_ts_offset", strconv.FormatFloat(startSec, 'f', 3, 64))
	}

	// Map video + selected audio. Always use first video stream.
	args = append(args, "-map", "0:v:0")
	audioIdx := cfg.AudioIndex
	if audioIdx < 0 {
		audioIdx = 0
		for i, a := range probe.AudioTracks {
			if a.Default {
				audioIdx = i
				break
			}
		}
	}
	args = append(args, "-map", fmt.Sprintf("0:a:%d?", audioIdx))

	// Video encode.
	codec := hwHint.FFmpegVideoCodec("h264")
	args = append(args, "-c:v", codec)
	// Encoder-specific tuning. Each HW encoder takes a different "preset"
	// vocabulary; libx264 uses ultrafast→placebo, NVENC uses p1→p7, QSV uses
	// veryfast→veryslow, VAAPI/VideoToolbox don't expose presets.
	switch codec {
	case "libx264":
		preset := cfg.Transcode.Preset
		if preset == "" {
			preset = "veryfast"
		}
		args = append(args, "-preset", preset)
	case "h264_nvenc":
		// p4 = balanced quality/speed; p1 fastest, p7 highest quality.
		args = append(args, "-preset", "p4", "-rc", "vbr", "-tune", "hq")
	case "h264_qsv":
		args = append(args, "-preset", "medium", "-look_ahead", "0")
	}
	// Derive H.264 level from the actual output height. A fixed "4.0" caps the
	// encoder at 1080p — anything taller (1440p, 4K source on quality=original)
	// fails libx264 with "frame MB size > level limit" and emits unplayable
	// segments. The output height matches qcap.MaxHeight when the source is
	// downscaled, otherwise probe.Height (already populated by ffprobe).
	qcap := resolveQualityCap(cfg.Quality)
	outputHeight := qcap.MaxHeight
	if outputHeight == 0 {
		outputHeight = cfg.Transcode.MaxHeight
	}
	if outputHeight == 0 || (probe.Height > 0 && probe.Height < outputHeight) {
		outputHeight = probe.Height
	}
	args = append(args, "-profile:v", "main", "-level:v", H264LevelForHeight(outputHeight))

	// Bitrate must match the level libx264 actually picks for outputHeight,
	// not the qcap target for the user's requested label. If a user asks for
	// "2160p" on a 1080p source, qcap.VideoBitrate is 25 Mbps but the level
	// (derived from outputHeight=1080) is 4.0, which rejects bitrates >20 Mbps
	// with "VBV bitrate (25000) > level limit (20000)". Re-derive the cap
	// from the effective height so the (level, bitrate) pair stays coherent.
	effectiveCap := capForHeight(outputHeight)
	bitrate := effectiveCap.VideoBitrate
	if bitrate == "" {
		bitrate = qcap.VideoBitrate
	}
	if bitrate == "" {
		bitrate = cfg.Transcode.VideoBitrate
	}
	if bitrate == "" {
		bitrate = "5M"
	}
	args = append(args, "-b:v", bitrate, "-maxrate", bitrate, "-bufsize", bitrate)

	// Force keyframe alignment with segment boundaries.
	args = append(args, "-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", hlsSegmentDuration))

	// Filter chain: optional scale, force 8-bit yuv420p, normalise color metadata.
	//
	// Width-rounding pitfall: `scale=-2:H` alone derives width from `H * dar` and
	// rounds to the nearest multiple of 2, which is correct. But adding
	// `force_original_aspect_ratio=decrease` makes ffmpeg ignore the `-2` and
	// emit the exact computed width — which can be odd (e.g. 853×480) and
	// libx264 then refuses to open. We chain a second `scale=trunc(iw/2)*2:...`
	// after the cap to guarantee even dimensions before format/setparams.
	maxH := qcap.MaxHeight
	if maxH == 0 {
		maxH = cfg.Transcode.MaxHeight
	}
	var filterChain string
	if maxH > 0 && probe.Height > maxH {
		filterChain = fmt.Sprintf(
			"scale=-2:%d:force_original_aspect_ratio=decrease,scale=trunc(iw/2)*2:trunc(ih/2)*2,format=yuv420p,setparams=colorspace=bt709:color_trc=bt709:color_primaries=bt709:range=tv",
			maxH,
		)
	} else {
		filterChain = "scale=trunc(iw/2)*2:trunc(ih/2)*2,format=yuv420p,setparams=colorspace=bt709:color_trc=bt709:color_primaries=bt709:range=tv"
	}
	args = append(args, "-vf", filterChain)

	// Audio: AAC stereo 48 kHz — broadest browser compatibility.
	audioBitrate := cfg.Transcode.AudioBitrate
	if audioBitrate == "" {
		audioBitrate = "192k"
	}
	args = append(args,
		"-c:a", "aac",
		"-b:a", audioBitrate,
		"-ar", "48000",
		"-ac", "2",
	)

	// HLS muxer — fmp4 segments with pre-computed segment count.
	// `-start_number` slots seg-N.m4s where N matches the segment index in
	// the pre-rendered manifest. Each ffmpeg writes its own ffmpeg.m3u8 but
	// we never serve it — the rendered VOD manifest already knows everything.
	videoDir := filepath.Join(tmpDir, "video")
	manifestName := fmt.Sprintf("ffmpeg-%d.m3u8", startIdx)
	args = append(args,
		"-f", "hls",
		"-hls_time", strconv.Itoa(hlsSegmentDuration),
		"-hls_playlist_type", "vod",
		"-hls_segment_type", "fmp4",
		"-hls_list_size", "0",
		"-hls_flags", "independent_segments",
		"-start_number", strconv.Itoa(startIdx),
		"-hls_fmp4_init_filename", "init.mp4",
		"-hls_segment_filename", filepath.Join(videoDir, "seg-%d.m4s"),
		filepath.Join(videoDir, manifestName),
	)
	return args
}

// extractSubtitles spawns short-lived ffmpeg jobs to convert each text-based
// subtitle track to WebVTT in parallel. Bitmap subs (PGS, DVB) are skipped —
// they would require burn-in into the video encode, which is out of scope.
func (s *HLSSession) extractSubtitles(ctx context.Context) {
	subsDir := filepath.Join(s.tmpDir, "subs")
	for i, sub := range s.probe.SubtitleTracks {
		if !sub.IsTextSubtitle() {
			continue
		}
		out := filepath.Join(subsDir, fmt.Sprintf("sub-%d.vtt", i))
		args := []string{
			"-y", "-hide_banner", "-loglevel", "warning",
			"-i", s.cfg.SourcePath,
			"-map", fmt.Sprintf("0:s:%d?", i),
			"-c:s", "webvtt",
			out,
		}
		// Run sequentially to avoid hammering the disk; subtitle extraction
		// is fast enough that parallelism isn't worth the complexity.
		cmd := exec.CommandContext(ctx, s.cfg.Transcode.FFmpegPath, args...)
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("[hls %s] subtitle %d (%s) extract failed: %v",
				shortHLSID(s.cfg.SessionID), i, sub.Lang, err)
			continue
		}
	}
}

// ---- Manifest rendering ----

// renderVideoPlaylist builds the VOD media playlist for the video stream.
// Segment count is derived from the source duration — the player learns the
// total timeline from the manifest before any segment is fetched.
func renderVideoPlaylist(durationSec float64, segCount int) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:7\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", hlsSegmentDuration+1))
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	b.WriteString(`#EXT-X-MAP:URI="init.mp4"` + "\n")
	remaining := durationSec
	for i := 0; i < segCount; i++ {
		segDur := float64(hlsSegmentDuration)
		if remaining < segDur {
			segDur = remaining
		}
		b.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", segDur))
		b.WriteString(fmt.Sprintf("seg-%d.m4s\n", i))
		remaining -= segDur
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

// renderMasterPlaylist builds the top-level master playlist with the single
// video variant + every text subtitle as an EXT-X-MEDIA group. Audio is muxed
// into the video segments for the MVP — separate audio renditions can come
// later (they require a second ffmpeg pipeline producing audio-only segments).
func renderMasterPlaylist(probe *StreamProbe, qualityLabel string) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:7\n")

	// Subtitle renditions. We never set DEFAULT=YES or AUTOSELECT=YES on any
	// rendition: anime files routinely ship a forced "signs only" English
	// track with cues only every few minutes, and stacking that track plus
	// the user's locale auto-select produced the "subs broken" report. The
	// HLS spec also caps DEFAULT to one per GROUP-ID — "none" trivially
	// satisfies it. Names disambiguate when several tracks share the same
	// language ("ES", "ES 2", forced suffix).
	hasSubs := false
	langCounts := make(map[string]int)
	for i, s := range probe.SubtitleTracks {
		if !s.IsTextSubtitle() {
			continue
		}
		hasSubs = true
		lang := s.Lang
		if lang == "" {
			lang = "und"
		}
		base := s.Title
		if base == "" {
			base = strings.ToUpper(lang)
		}
		key := strings.ToLower(base)
		langCounts[key]++
		name := base
		if langCounts[key] > 1 {
			name = fmt.Sprintf("%s %d", base, langCounts[key])
		}
		if s.Forced {
			name = name + " (forced)"
		}
		b.WriteString(fmt.Sprintf(
			`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME=%q,LANGUAGE=%q,DEFAULT=NO,AUTOSELECT=NO,FORCED=%s,URI="subs/sub-%d.m3u8"`+"\n",
			name, lang, ynBool(s.Forced), i,
		))
	}

	// Video variant. Bandwidth + resolution are best-effort estimates from probe.
	bw := bitrateForQuality(qualityLabel)
	w, h := scaledDimensions(probe.Width, probe.Height, qualityHeight(qualityLabel))
	codecs := `avc1.4D4028,mp4a.40.2`
	streamInf := fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%dx%d,CODECS=%q", bw, w, h, codecs)
	if hasSubs {
		streamInf += `,SUBTITLES="subs"`
	}
	b.WriteString(streamInf + "\n")
	b.WriteString("video/index.m3u8\n")
	return b.String()
}

func ynBool(b bool) string {
	if b {
		return "YES"
	}
	return "NO"
}

// bitrateForQuality returns a synthetic bandwidth attribute for the master
// playlist's STREAM-INF — only used by ABR logic, which we don't run yet.
func bitrateForQuality(q string) int {
	switch q {
	case "2160p":
		return 25_000_000
	case "1080p":
		return 6_000_000
	case "720p":
		return 3_500_000
	case "480p":
		return 1_500_000
	}
	return 6_000_000
}

func qualityHeight(q string) int {
	switch q {
	case "2160p":
		return 2160
	case "1080p":
		return 1080
	case "720p":
		return 720
	case "480p":
		return 480
	}
	return 0
}

// scaledDimensions returns (width, height) after applying a height cap that
// preserves the source aspect ratio. capH=0 returns the original dims.
func scaledDimensions(srcW, srcH, capH int) (int, int) {
	if srcW <= 0 || srcH <= 0 {
		return 1920, 1080
	}
	if capH == 0 || srcH <= capH {
		return srcW, srcH
	}
	w := srcW * capH / srcH
	if w%2 != 0 {
		w++
	}
	return w, capH
}

// ---- Logger plumbing ----

// hlsStderrCapture forwards ffmpeg stderr lines to the daemon log prefixed by
// the session ID, so failures are visible without spelunking tmpdirs.
//
// The internal buffer accumulates partial bytes between writes (a single line
// can span multiple Write calls). Capped at maxStderrBuf so a misbehaving
// ffmpeg that emits megabytes without newlines can't grow daemon memory
// unbounded; on overflow we discard the partial line and keep going.
type hlsStderrCapture struct {
	owner *HLSSession
	buf   strings.Builder
}

const maxStderrBuf = 64 * 1024

func (c *hlsStderrCapture) Write(p []byte) (int, error) {
	// If the incoming chunk alone exceeds the cap (very long unterminated
	// line), drop the buffered prefix AND truncate p so a single multi-MB
	// write can't grow memory.
	if len(p) > maxStderrBuf {
		c.buf.Reset()
		p = p[len(p)-maxStderrBuf:]
	} else if c.buf.Len()+len(p) > maxStderrBuf {
		// Drop the unterminated partial line; we'll resync on the next \n.
		c.buf.Reset()
	}
	c.buf.Write(p)
	for {
		line, rest, ok := strings.Cut(c.buf.String(), "\n")
		if !ok {
			break
		}
		c.buf.Reset()
		c.buf.WriteString(rest)
		if line = strings.TrimSpace(line); line != "" {
			log.Printf("[hls %s] ffmpeg: %s", shortHLSID(c.owner.cfg.SessionID), line)
		}
	}
	return len(p), nil
}

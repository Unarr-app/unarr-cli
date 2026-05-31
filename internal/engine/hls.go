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

// hlsSegmentDuration is the target seconds per HLS fragment.
//
// We use 2 seconds (not the more common 4-6 s). Trade-off: 2× more segments
// per source (a 2 h movie produces 3600 segments instead of 1800), but the
// player's first-frame wait drops to ~half — ffmpeg only needs to encode
// 2 s before seg-0 lands. For software encodes on 4K this is ~1 s instead
// of ~3 s of cold-cache wait. Well within HLS spec (Apple recommends 6 s,
// but 2-6 s is acceptable; Low-Latency HLS uses 1-2 s segments).
//
// Caveat for existing cached encodes: cache entries from 0.9.9 used 4 s
// segments. After this bump, VerifyComplete (which checks the highest
// expected segment index) returns false for those entries — they're
// invalidated + re-encoded with 2 s segments on next play. Self-healing.
const hlsSegmentDuration = 2

// segmentDurationFor returns the target duration (in whole seconds) for the
// segment at index idx. With uniform-duration segments this is always
// hlsSegmentDuration; the helper exists so a future short-first-segment
// variant can be slotted in here without touching every call site.
func segmentDurationFor(idx int) int {
	return hlsSegmentDuration
}

// segmentStartSec returns the wall-clock start time of segment idx. Used
// to compute the `-ss` flag when ffmpeg restarts at a mid-file segment.
func segmentStartSec(idx int) float64 {
	if idx <= 0 {
		return 0
	}
	return float64(idx * hlsSegmentDuration)
}

// segmentCountForDuration returns how many segments cover a source of the
// given duration. Always returns at least 1.
func segmentCountForDuration(dur float64) int {
	if dur <= 0 {
		return 1
	}
	return int((dur + float64(hlsSegmentDuration) - 1) / float64(hlsSegmentDuration))
}

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
	SessionID string
	// Exactly one of SourcePath / SourceURL identifies the input. SourcePath is
	// a local file; SourceURL is a remote HTTP(S) URL ffmpeg reads directly
	// (hueco #2 / 2b — transcoding a debrid source that isn't browser-native).
	SourcePath string
	// SourceURL, when set, is fed to ffmpeg/ffprobe as the input (-i <url>) with
	// network-resilience flags. Takes priority over SourcePath.
	SourceURL string
	// CacheID overrides the cache key identity. Empty → key by SourcePath (local
	// files). Set to a stable id (the torrent info_hash) for SourceURL sessions
	// so re-plays cache-hit even though the debrid URL changes each resolution.
	CacheID string
	// RefreshURL, when set (debrid URL sessions only), re-resolves a fresh
	// SourceURL when the current link expires mid-transcode (hueco #2 / 2c).
	// The auto-restart supervisor calls it before relaunching ffmpeg so the
	// restart uses a live link instead of retrying the dead one. nil = no refresh.
	RefreshURL func(context.Context) (string, error)
	FileName   string
	Quality    string // "2160p"|"1080p"|"720p"|"480p"|"original"|""
	AudioIndex int    // 0-based ffmpeg audio stream selection (-map 0:a:N). -1 = default.
	Transcode  TranscodeRuntime
	// Cache is an optional persistent segment cache keyed by (source, quality,
	// audio). When set, completed encodes are kept across sessions so re-plays
	// of the same file at the same quality skip ffmpeg entirely. nil disables
	// caching (per-session tmpdir, deleted on Close — original behavior).
	Cache *HLSCache
}

// sourceRef returns the ffmpeg/ffprobe input: the remote URL when set, else the
// local path. Used everywhere a `-i` argument or a probe target is needed so
// the local-file and debrid-URL paths share one code path.
func (cfg HLSSessionConfig) sourceRef() string {
	if cfg.SourceURL != "" {
		return cfg.SourceURL
	}
	return cfg.SourcePath
}

// logName is a short, log-friendly source label. For local files it's the base
// name; for a URL source (no SourcePath) it prefers FileName over the raw URL
// (which would leak a query-string token into the logs).
func (cfg HLSSessionConfig) logName() string {
	if cfg.SourcePath != "" {
		return filepath.Base(cfg.SourcePath)
	}
	if cfg.FileName != "" {
		return cfg.FileName
	}
	return "debrid-url"
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
	// liveURL is the mutable debrid source URL (hueco #2 / 2c). Initialised to
	// cfg.SourceURL; refreshed in place by waitFFmpeg when the link expires.
	// Guarded by mu because restartFromSegment reads it from BOTH the supervisor
	// goroutine (auto-restart) AND the HTTP handler goroutine (seek-restart),
	// while waitFFmpeg writes it. Empty for local-file sessions. cfg itself is
	// treated as immutable after construction so copying it stays race-free.
	liveURL string

	// readyCh + readyMax track how many segments ffmpeg has finished writing.
	// readyMax is a COUNT (not an index): readyMax=N means seg-0 … seg-(N-1)
	// are fully on disk. A handler waiting on `idx` blocks until
	// `idx < readyMax` (segment idx is present). The pollSegments goroutine
	// advances readyMax and re-creates readyCh on every step.
	readyMu  sync.Mutex
	readyMax int
	exitErr  error
	exited   bool
	readyCh  chan struct{} // closed + replaced each time readyMax advances

	// Persistent cache state. cache==nil means caching disabled for this session.
	// fromCache=true means the session is replaying a completed encode and no
	// ffmpeg subprocess was spawned. writerLockHeld=true means this session
	// owns the per-key TryAcquireWriter claim — Close must ReleaseWriter.
	// subsDone closes when the subtitle extractor goroutine returns (or is
	// nil when the source had no subtitle tracks); MarkComplete waits on it
	// so a HIT replay never serves partial .vtt files.
	cache          *HLSCache
	cacheKey       string
	fromCache      bool
	writerLockHeld bool
	subsDone       chan struct{}
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
	if cfg.SourcePath == "" && cfg.SourceURL == "" {
		return nil, errors.New("hls: no source (neither path nor URL)")
	}
	if cfg.Transcode.FFmpegPath == "" || cfg.Transcode.FFprobePath == "" {
		return nil, errors.New("hls: ffmpeg/ffprobe not available")
	}

	// Probe gets a 15 s ceiling. ffprobe on a 50 GB MKV over a slow remote
	// fs can hang indefinitely; without a deadline the daemon would block
	// the goroutine that started the session forever and the user would
	// see the player phase stuck on "Preparando sesión".
	probeCtx, cancelProbe := context.WithTimeout(ctx, 15*time.Second)
	probe, err := ProbeFile(probeCtx, cfg.Transcode.FFprobePath, cfg.sourceRef())
	cancelProbe()
	if err != nil {
		return nil, fmt.Errorf("hls: probe: %w", err)
	}
	if probe.DurationSec <= 0 {
		return nil, errors.New("hls: source has no duration")
	}

	// Resolve tmpDir + cache placement. Three states:
	//   1. cache disabled              → per-session tmpdir, deleted on Close.
	//   2. cache HIT (.complete found) → read from cache dir, no ffmpeg, Pin.
	//   3. cache MISS, writer-lock OK  → ffmpeg writes to cache dir, Pin + writer-lock.
	//   4. cache MISS, writer-lock NO  → another session already writing this
	//                                    key; fall back to private per-session tmpdir
	//                                    (no caching for this session — second-writer
	//                                    would corrupt the first one's segments).
	var (
		tmpDir         string
		cacheKey       string
		fromCache      bool
		writerLockHeld bool
	)
	if cfg.Cache != nil {
		// Debrid URL sessions key by CacheID (info_hash) so re-plays hit cache
		// despite the URL changing each resolution; local files key by path.
		if cfg.CacheID != "" {
			cacheKey = cfg.Cache.KeyForID(cfg.CacheID, cfg.Quality, cfg.AudioIndex)
		} else {
			cacheKey = cfg.Cache.KeyFor(cfg.SourcePath, cfg.Quality, cfg.AudioIndex)
		}
		// Integrity gate: HasComplete just stats the marker. If init.mp4 or
		// the last segment vanished (external rm, partial-disk failure), we
		// can't actually serve a HIT — drop the dir and re-encode.
		segCountForVerify := segmentCountForDuration(probe.DurationSec)
		if cfg.Cache.HasComplete(cacheKey) && !cfg.Cache.VerifyComplete(cacheKey, segCountForVerify) {
			log.Printf("[hls %s] cache %s sealed but failed integrity check — re-encoding",
				shortHLSID(cfg.SessionID), cacheKey)
			_ = cfg.Cache.Invalidate(cacheKey)
		}
		if cfg.Cache.HasComplete(cacheKey) {
			// HIT: read-only replay — many concurrent HITs are fine.
			tmpDir = cfg.Cache.DirFor(cacheKey)
			cfg.Cache.Pin(cacheKey)
			fromCache = true
			cfg.Cache.RecordHit()
			_ = cfg.Cache.Touch(cacheKey)
		} else if cfg.Cache.TryAcquireWriter(cacheKey) {
			tmpDir = cfg.Cache.DirFor(cacheKey)
			cfg.Cache.Pin(cacheKey)
			writerLockHeld = true
			cfg.Cache.RecordMiss()
		} else {
			// Another session is writing this key — fall back to private
			// dir so we don't trample its segments.
			log.Printf("[hls %s] cache key %s busy, falling back to per-session tmpdir",
				shortHLSID(cfg.SessionID), cacheKey)
			tmpDir = filepath.Join(hlsTmpDirRoot(), cfg.SessionID)
			cacheKey = "" // disable caching for this session
			cfg.Cache.RecordMiss()
		}
	} else {
		tmpDir = filepath.Join(hlsTmpDirRoot(), cfg.SessionID)
	}

	cleanupOnError := func() {
		if cfg.Cache != nil && cacheKey != "" {
			cfg.Cache.Unpin(cacheKey)
			if writerLockHeld {
				cfg.Cache.ReleaseWriter(cacheKey)
				_ = cfg.Cache.Invalidate(cacheKey)
			}
		} else {
			_ = os.RemoveAll(tmpDir)
		}
	}

	if err := os.MkdirAll(filepath.Join(tmpDir, "video"), 0o755); err != nil {
		cleanupOnError()
		return nil, fmt.Errorf("hls: mkdir video: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "subs"), 0o755); err != nil {
		cleanupOnError()
		return nil, fmt.Errorf("hls: mkdir subs: %w", err)
	}

	segCount := segmentCountForDuration(probe.DurationSec)

	s := &HLSSession{
		cfg:            cfg,
		probe:          probe,
		tmpDir:         tmpDir,
		durationSec:    probe.DurationSec,
		segmentCount:   segCount,
		startedAt:      time.Now(),
		lastTouch:      time.Now(),
		readyCh:        make(chan struct{}),
		cache:          cfg.Cache,
		cacheKey:       cacheKey,
		fromCache:      fromCache,
		writerLockHeld: writerLockHeld,
		liveURL:        cfg.SourceURL, // mutable copy; cfg stays immutable
	}
	s.manifestVideo = renderVideoPlaylist(probe.DurationSec, segCount)
	s.manifestRoot = renderMasterPlaylist(probe, cfg.Quality)

	// Cache HIT: every segment + init.mp4 is already on disk. Skip ffmpeg
	// entirely and mark readyMax so handlers don't wait. Background subtitle
	// extraction is also unnecessary — subs were extracted on the original run.
	if fromCache {
		s.readyMu.Lock()
		s.readyMax = segCount - 1
		s.exited = true
		close(s.readyCh)
		s.readyCh = nil
		s.readyMu.Unlock()
		log.Printf("[hls %s] cache HIT %s: %s, %.1fs, %d segs (quality=%s)",
			shortHLSID(cfg.SessionID), cacheKey, cfg.logName(),
			probe.DurationSec, segCount, coalesce(cfg.Quality, "auto"))
		return s, nil
	}

	// Spawn ffmpeg under a dedicated context so Close() can kill it without
	// touching the parent ctx.
	ffCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	args := buildHLSFFmpegArgs(cfg, probe, tmpDir)
	cmd := exec.CommandContext(ffCtx, cfg.Transcode.FFmpegPath, args...)
	cmd.Stderr = &hlsStderrCapture{owner: s}
	if err := cmd.Start(); err != nil {
		cancel()
		cleanupOnError()
		return nil, fmt.Errorf("hls: start ffmpeg: %w", err)
	}
	s.cmd = cmd

	go s.waitFFmpeg()
	go s.pollSegments(ffCtx)

	if len(probe.SubtitleTracks) > 0 {
		s.subsDone = make(chan struct{})
		// Capture the source ref now (by value): subs are extracted once at
		// startup, and a later URL refresh (2c) mutates s.cfg.SourceURL from the
		// waitFFmpeg goroutine — passing the URL in keeps extractSubtitles from
		// racing that write.
		subSrc := cfg.sourceRef()
		go func() {
			defer close(s.subsDone)
			s.extractSubtitles(ffCtx, subSrc)
		}()
	}

	cachedNote := ""
	if cfg.Cache != nil {
		cachedNote = fmt.Sprintf(" (cache-miss %s)", cacheKey)
	}
	// Surface the encoder profile so a "first-start was slow" report can be
	// triaged from the agent log alone — `encoder=libx264 accel=none` means
	// the user's ffmpeg has no HW encoders compiled in, which is the most
	// common root cause (linuxbrew, default brew formula on macOS).
	profile := ResolveEncoderProfile(cfg.Transcode.HWAccel, cfg.Transcode.Preset)
	presetNote := ""
	if profile.Preset != "" {
		presetNote = " preset=" + profile.Preset
	}
	log.Printf("[hls %s] started: %s, %.1fs, %d segs (quality=%s, encoder=%s accel=%s%s)%s",
		shortHLSID(cfg.SessionID), cfg.logName(),
		probe.DurationSec, segCount, coalesce(cfg.Quality, "auto"),
		profile.Codec, string(cfg.Transcode.HWAccel), presetNote, cachedNote)
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

// ReadyCount returns how many segments are currently fully on disk.
// Caller can `>= 1` it to check whether seg-0 has landed (and so the
// player can be told to attach). For cache-HIT sessions this is always
// `segmentCount` from the moment StartHLSSession returns.
func (s *HLSSession) ReadyCount() int {
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	return s.readyMax
}

// FromCache reports whether this session was served from the HLS cache
// (no ffmpeg subprocess spawned). Used by ready-watcher logic to short-
// circuit polling — a cache HIT is ready the moment we return.
func (s *HLSSession) FromCache() bool { return s.fromCache }

// IsClosed reports whether Close() has been invoked. Exposed (vs the
// internal isClosed) so external watchers — the ready-webhook
// goroutine in cmd/daemon.go — can short-circuit polling on a session
// that was torn down through a different code path (registry replace,
// idle sweep) without racing on the unexported helper.
func (s *HLSSession) IsClosed() bool { return s.isClosed() }

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

// Close stops ffmpeg and prevents further requests from blocking on segment
// readiness. Idempotent.
//
// Disk lifecycle:
//   - cache disabled → delete tmpDir (original behavior).
//   - cache enabled + this session was a HIT → keep dir, just unpin.
//   - cache enabled + this was a write session → if ffmpeg exited cleanly and
//     every segment is on disk, persist with .complete and keep dir. Otherwise
//     drop the dir so a half-written cache doesn't survive into the next play.
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
	exitErr := s.exitErr
	s.readyMu.Unlock()

	if s.cache != nil && s.cacheKey != "" {
		defer s.cache.Unpin(s.cacheKey)
		if s.writerLockHeld {
			defer s.cache.ReleaseWriter(s.cacheKey)
		}
		if s.fromCache {
			log.Printf("[hls %s] closed (cache reuse)", shortHLSID(s.cfg.SessionID))
			return nil
		}
		// Wait briefly for the subtitle extractor to finish so a cached
		// replay never serves half-written .vtt files. Bounded so a stuck
		// extractor can't block Close indefinitely; on timeout we treat
		// the cache as incomplete and drop it.
		subsOK := true
		if s.subsDone != nil {
			select {
			case <-s.subsDone:
			case <-time.After(15 * time.Second):
				log.Printf("[hls %s] subtitle extractor timeout — not caching", shortHLSID(s.cfg.SessionID))
				subsOK = false
			}
		}
		if subsOK && exitErr == nil && s.allSegmentsPresent() {
			if err := s.cache.MarkComplete(s.cacheKey); err == nil {
				log.Printf("[hls %s] cache persisted %s", shortHLSID(s.cfg.SessionID), s.cacheKey)
				return nil
			} else {
				log.Printf("[hls %s] cache persist failed: %v", shortHLSID(s.cfg.SessionID), err)
			}
		}
		// Partial / failed → drop so we re-encode next time.
		if err := s.cache.Invalidate(s.cacheKey); err != nil {
			log.Printf("[hls %s] cache invalidate failed: %v", shortHLSID(s.cfg.SessionID), err)
		}
		log.Printf("[hls %s] closed (cache discarded)", shortHLSID(s.cfg.SessionID))
		return nil
	}

	if tmpDir != "" {
		_ = os.RemoveAll(tmpDir)
	}
	log.Printf("[hls %s] closed", shortHLSID(s.cfg.SessionID))
	return nil
}

// allSegmentsPresent reports whether every expected segment (and init.mp4) is
// on disk AND validated by the segment poller. Used to decide whether a
// finished session is cacheable. We trust readyMax (advanced by pollSegments
// only after the next segment exists, proving the predecessor is fully closed)
// over a naive Size>0 stat that could accept truncated mid-write files.
func (s *HLSSession) allSegmentsPresent() bool {
	if fi, err := os.Stat(filepath.Join(s.tmpDir, "video", "init.mp4")); err != nil || fi.Size() == 0 {
		return false
	}
	s.readyMu.Lock()
	readyMax := s.readyMax
	s.readyMu.Unlock()
	if readyMax < s.segmentCount-1 {
		return false
	}
	for i := 0; i < s.segmentCount; i++ {
		path := filepath.Join(s.tmpDir, "video", fmt.Sprintf("seg-%d.m4s", i))
		fi, err := os.Stat(path)
		if err != nil || fi.Size() == 0 {
			return false
		}
	}
	return true
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

	// Debrid URL session (hueco #2 / 2c): the likeliest cause of an ffmpeg
	// network exit is the debrid link expiring. Re-resolve a fresh one before
	// restarting, else the restart just retries the dead URL and burns the
	// retry budget. The network call runs lock-free; the result is stored in
	// s.liveURL under s.mu because restartFromSegment reads it from the HTTP
	// handler goroutine too (seek-restart), not just this supervisor goroutine.
	if s.cfg.SourceURL != "" && s.cfg.RefreshURL != nil {
		rctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		newURL, rerr := s.cfg.RefreshURL(rctx)
		cancel()
		if rerr != nil {
			log.Printf("[hls %s] URL refresh before restart failed: %v", shortHLSID(s.cfg.SessionID), rerr)
		} else {
			s.mu.Lock()
			s.liveURL = newURL
			s.mu.Unlock()
			log.Printf("[hls %s] debrid URL refreshed before restart", shortHLSID(s.cfg.SessionID))
		}
	}

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

	// Build args for the new ffmpeg with -ss offset. Segments are non-uniform
	// (seg-0 is hlsInitSegmentDuration s, the rest are hlsSegmentDuration s),
	// so use segmentStartSec for the seek time instead of multiplying.
	// Use a local cfg copy carrying the live (possibly-refreshed) debrid URL,
	// read under s.mu — this runs from the HTTP handler goroutine too, so it
	// can't read s.liveURL unsynchronised while waitFFmpeg writes it (2c).
	startSec := segmentStartSec(targetIdx)
	cfg := s.cfg
	s.mu.Lock()
	cfg.SourceURL = s.liveURL // "" for local-file sessions — no-op, sourceRef falls back to SourcePath
	s.mu.Unlock()
	args := buildHLSFFmpegArgsAt(cfg, s.probe, s.tmpDir, targetIdx, startSec)

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

// EncoderProfile names the codec + preset + decoder hint combination the HLS
// pipeline picks for the given hardware backend + transcode config. Exposed
// so callers can log the chosen encoder before ffmpeg launches and so both
// the demuxer-side `-hwaccel` flag and the encoder-side argv stay in sync
// (otherwise the two switches in buildHLSFFmpegArgsAt could silently drift
// when adding a new backend).
type EncoderProfile struct {
	Codec         string // ffmpeg encoder name (e.g. "h264_nvenc", "libx264")
	Preset        string // preset string, or "" when the codec has no preset knob
	DecodeHwAccel string // ffmpeg `-hwaccel` value (e.g. "cuda", "qsv", "vaapi"), or ""
}

// ResolveEncoderProfile mirrors the codec + preset selection inside
// buildHLSFFmpegArgsAt so callers (registry, log lines, diagnostic
// endpoints) can know what ffmpeg will be told to do without parsing argv.
//
// The configured preset is libx264-specific by vocabulary (ultrafast…
// veryslow). Passing it through to NVENC / QSV would have ffmpeg reject
// the argv (NVENC uses p1-p7, QSV uses its own subset). So vendor encoders
// always use their hardcoded vendor preset and ignore configuredPreset.
// VideoToolbox has no preset knob at all.
//
// DecodeHwAccel mirrors the encoder family — `-hwaccel cuda` for NVENC,
// `-hwaccel qsv` for QSV, `-hwaccel vaapi` for VAAPI. We intentionally
// do NOT pass `-hwaccel_output_format vaapi`: that pins decoded frames
// to GPU memory, but our filter chain (scale/format/setparams) runs on
// CPU and can't consume VAAPI surfaces. Keeping output frames on CPU
// makes the filter chain work and the VAAPI encoder still benefits from
// HW-accelerated DECODE on the input side.
func ResolveEncoderProfile(hw HWAccel, configuredPreset string) EncoderProfile {
	codec := hw.FFmpegVideoCodec("h264")
	switch codec {
	case "libx264":
		preset := configuredPreset
		if preset == "" {
			preset = "superfast"
		}
		return EncoderProfile{Codec: codec, Preset: preset, DecodeHwAccel: ""}
	case "h264_nvenc":
		return EncoderProfile{Codec: codec, Preset: "p3", DecodeHwAccel: "cuda"}
	case "h264_qsv":
		return EncoderProfile{Codec: codec, Preset: "veryfast", DecodeHwAccel: "qsv"}
	case "h264_vaapi":
		return EncoderProfile{Codec: codec, Preset: "", DecodeHwAccel: "vaapi"}
	case "h264_videotoolbox":
		// No preset knob for VideoToolbox; the speed/quality dial is `-q:v`.
		// VideoToolbox uses per-encoder flags rather than a demuxer hint.
		return EncoderProfile{Codec: codec, Preset: "", DecodeHwAccel: ""}
	}
	// Unknown / future codecs: software path.
	return EncoderProfile{Codec: codec, Preset: "", DecodeHwAccel: ""}
}

// buildHLSFFmpegArgsAt returns the argv for an HLS encode that starts at the
// given segment index (`-ss <startSec>`) and writes segments numbered from
// startIdx so they slot into the existing manifest at the correct position.
// `-output_ts_offset` keeps the segment PTS aligned with manifest timeline.
func buildHLSFFmpegArgsAt(cfg HLSSessionConfig, probe *StreamProbe, tmpDir string, startIdx int, startSec float64) []string {
	profile := ResolveEncoderProfile(cfg.Transcode.HWAccel, cfg.Transcode.Preset)
	args := []string{"-y", "-hide_banner", "-loglevel", "warning"}

	// Demuxer-side HW-decode hint. Sourced from the profile so a future
	// codec/hint mismatch is impossible — the encoder + decode hint are
	// computed once and stay coherent. Notably we do NOT add
	// `-hwaccel_output_format vaapi` on the VAAPI path: that pins decoded
	// frames to GPU memory but our CPU filter chain (scale, format,
	// setparams) can't consume VAAPI surfaces. Letting frames flow on CPU
	// keeps the filter chain working; the encoder still gets HW-accelerated
	// decode on the input side.
	if profile.DecodeHwAccel != "" {
		args = append(args, "-hwaccel", profile.DecodeHwAccel)
	}

	// Seek before -i for fast keyframe-aligned start. The new ffmpeg writes
	// segments with PTS shifted via -output_ts_offset so the manifest's
	// pre-computed segment numbering still matches the timeline.
	if startSec > 0 {
		args = append(args, "-ss", strconv.FormatFloat(startSec, 'f', 3, 64))
	}

	// Remote (debrid) input: make the HTTP read resilient. -reconnect* recovers
	// from a dropped/idle connection (debrid CDNs close long-idle sockets);
	// -rw_timeout (µs) bounds a stalled read so a hung CDN surfaces as a restart
	// instead of a frozen player. A seek (-ss before -i) re-opens the URL with a
	// Range request, which debrid supports. Flags are no-ops for local files, so
	// only add them for a URL source.
	if cfg.SourceURL != "" {
		args = append(args,
			"-reconnect", "1",
			"-reconnect_streamed", "1",
			"-reconnect_delay_max", "5",
			"-rw_timeout", "30000000",
		)
	}

	args = append(args, "-i", cfg.sourceRef())

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

	// Video encode. Codec + preset come from the EncoderProfile resolved at
	// the top of this function so the demuxer hint, the encoder, and the
	// per-session log line all stay consistent.
	//
	// Defaults are biased for FIRST-START LATENCY over quality — the player
	// blocks on seg-0 before the first frame paints, and a slow seg-0 is
	// what users notice ("preparando sesión" stuck). Users who want better
	// quality can override via `download.transcode.preset` in config.toml.
	codec := profile.Codec
	args = append(args, "-c:v", codec)
	switch codec {
	case "libx264":
		// superfast = ~15-20% faster than veryfast at marginal quality loss
		// for the bitrates we target (5-25 Mbps). For 4K software encodes
		// this is the difference between ~3 s and ~2.5 s per segment on a
		// recent x86 CPU. `-threads 0` is libx264's default but explicit
		// helps when the user has set GOMAXPROCS.
		args = append(args, "-preset", profile.Preset, "-threads", "0")
	case "h264_nvenc":
		// p3 + vbr keeps NVENC fast (~1.5 s seg-0) without the segmentation
		// breakage `-tune ll` introduced in 0.9.9: with -tune=ll the NVENC
		// rate control emits long IDR-less GOPs that ignore -force_key_frames,
		// so ffmpeg's HLS muxer never closes seg-0 and the player stalls at
		// "preparando sesión" until the 60 s mark-ready timeout. Verified on
		// ffmpeg 6.1.1 + driver 580 / RTX-class GPUs: dropping -tune ll
		// restores per-segment cuts at 27x real-time vs 28x with -tune ll.
		args = append(args, "-preset", profile.Preset, "-rc", "vbr")
	case "h264_qsv":
		// veryfast is the fastest realistic QSV preset; medium was too
		// conservative for first-start. look_ahead=0 keeps the encoder
		// truly low-latency (no rate-control look-ahead window).
		args = append(args, "-preset", profile.Preset, "-look_ahead", "0")
	case "h264_videotoolbox":
		// VideoToolbox has no "preset" knob; `-realtime` flips into the
		// low-latency path used by FaceTime. We let the `-b:v / -maxrate
		// / -bufsize` block (added later in this function) drive rate
		// control — adding `-q:v` here would conflict because ffmpeg's
		// videotoolbox encoder treats `-b:v` as authoritative and
		// silently ignores `-q:v`, so the constant-quality knob never
		// took effect anyway.
		args = append(args, "-realtime", "1")
	case "h264_vaapi":
		// h264_vaapi has no preset knob. Bitrate args (set later) drive
		// rate control. Add `-vaapi_device /dev/dri/renderD128` so the
		// encoder doesn't fall back to a NULL device on multi-GPU hosts
		// where the default render node is a non-VAAPI GPU (an Nvidia
		// dGPU's render node, etc.). The filter chain below switches to
		// `format=nv12,hwupload` so frames land on the right VAAPI
		// surface before the encoder; we intentionally avoid scale_vaapi
		// because mesa 25 + Raphael iGPU emits "Cannot allocate memory"
		// per session start, polluting logs even though encode succeeds.
		args = append(args, "-vaapi_device", "/dev/dri/renderD128")
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
	// VAAPI needs frames as nv12 VAAPI surfaces before the encoder. We do
	// scale + format conversion on CPU then `hwupload` once at the end —
	// skips the mesa 25 + Raphael iGPU "Cannot allocate memory" log spam
	// that scale_vaapi triggers per-session-start while still delivering
	// the encoder a GPU surface. setparams is dropped because VAAPI
	// surfaces don't expose VUI fields the way libx264 does; the encoder
	// records its own color metadata via the source PTS chain.
	pixFormat := "yuv420p"
	hwUploadTail := ""
	colorTail := ",setparams=colorspace=bt709:color_trc=bt709:color_primaries=bt709:range=tv"
	if codec == "h264_vaapi" {
		pixFormat = "nv12"
		hwUploadTail = ",hwupload"
		colorTail = ""
	}
	// HDR→SDR tonemap, inserted after the scale (downscale-first = fewer pixels
	// to tonemap) and before format=. Only for an HDR source on a zscale-capable
	// ffmpeg; the trailing comma in hdrTonemapChain slots it in front of format=.
	tonemap := ""
	if probe.HDR != "" && cfg.Transcode.TonemapHDR {
		tonemap = hdrTonemapChain
	}
	var filterChain string
	if maxH > 0 && probe.Height > maxH {
		filterChain = fmt.Sprintf(
			"scale=-2:%d:force_original_aspect_ratio=decrease,scale=trunc(iw/2)*2:trunc(ih/2)*2,%sformat=%s%s%s",
			maxH, tonemap, pixFormat, colorTail, hwUploadTail,
		)
	} else {
		filterChain = fmt.Sprintf(
			"scale=trunc(iw/2)*2:trunc(ih/2)*2,%sformat=%s%s%s",
			tonemap, pixFormat, colorTail, hwUploadTail,
		)
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
func (s *HLSSession) extractSubtitles(ctx context.Context, src string) {
	subsDir := filepath.Join(s.tmpDir, "subs")
	for i, sub := range s.probe.SubtitleTracks {
		if !sub.IsTextSubtitle() {
			continue
		}
		out := filepath.Join(subsDir, fmt.Sprintf("sub-%d.vtt", i))
		args := []string{
			"-y", "-hide_banner", "-loglevel", "warning",
			"-i", src,
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
//
// seg-0 is the short init segment (hlsInitSegmentDuration s); seg-1 onward
// are hlsSegmentDuration s each. The last segment may be shorter than the
// nominal duration when (duration - init) doesn't divide evenly.
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
		segDur := float64(segmentDurationFor(i))
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

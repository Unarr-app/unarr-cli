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
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// segmentIdxForTime returns the index of the segment containing second `sec`
// of the timeline — the inverse of segmentStartSec. Used to translate a
// session's StartSec (resume position) into the segment the FIRST ffmpeg
// should start writing from.
func segmentIdxForTime(sec float64) int {
	if sec <= 0 {
		return 0
	}
	return int(sec / float64(hlsSegmentDuration))
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
	// BurnSubtitleIndex burns a BITMAP subtitle (PGS/DVB) at this 0-based
	// subtitle stream index into the video. nil = no burn (text subs are served
	// as separate WebVTT). A pointer (not int) so the zero value 0 — a valid
	// stream index — can't be mistaken for a burn request when a caller leaves
	// the field unset. Part of the cache key so a burned encode never collides
	// with the clean one. Forces the video re-encode the HLS path already does
	// to also composite the subtitle overlay.
	BurnSubtitleIndex *int
	// StartSec is the playback position (seconds) the viewer will start at —
	// the saved resume point, or the current position on a quality/audio
	// switch. When > 0 the FIRST ffmpeg spawns already seeked there
	// (`-ss` + `-output_ts_offset` + `-start_number`, the same flags as a
	// seek-restart), instead of encoding from segment 0 only to be
	// killed by an immediate seek-restart when the player asks for the resume
	// segment (double spawn, slow resume). 0 = start at the beginning.
	// Ignored on a cache HIT (every segment is already on disk).
	StartSec float64
	// Prewarm marks a background cache-fill session. The daemon defers its
	// encode until no live encode runs and registers it via RegisterKeep
	// (never evicting the viewer). It also lets a REAL session close stale
	// prewarms up front so the cache writer-lock is free for the viewer.
	Prewarm   bool
	Transcode TranscodeRuntime
	// Cache is an optional persistent segment cache keyed by (source, quality,
	// audio). When set, completed encodes are kept across sessions so re-plays
	// of the same file at the same quality skip ffmpeg entirely. nil disables
	// caching (per-session tmpdir, deleted on Close — original behavior).
	Cache *HLSCache
	// VideoCopy switches the session to HLS-copy mode: ffmpeg `-c:v copy`
	// (NEVER re-encodes video — I/O-bound, works on a GPU-less NAS), audio
	// copied when already AAC or re-encoded to AAC otherwise. This replaces
	// the fragile progressive-remux path (growing fMP4 over manual HTTP
	// Range) with the robust segmented transport every player handles
	// (hls.js + native iOS HLS). Differences from the encode mode, all
	// driven by "segments cut at the SOURCE's keyframes, so their durations
	// are unknown upfront":
	//   - the media playlist is ffmpeg's own (EVENT → ENDLIST), served from
	//     disk — not the pre-rendered uniform-2s VOD manifest;
	//   - no seek-restart / auto-restart (copy outruns any viewer: the whole
	//     file is remuxed at I/O speed, minutes at worst on a weak NAS);
	//   - no HLS cache (re-generating costs no encode — caching would only
	//     burn disk);
	//   - StartSec is ignored: copy produces from 0 (outruns playback at I/O
	//     speed); an offset EVENT playlist breaks iOS's native HLS parser.
	// See docs/plans/hls-copy-remux-replacement.md (web repo).
	VideoCopy bool
}

// copyPlaylistName is the on-disk media playlist ffmpeg owns in VideoCopy
// mode, under <tmpDir>/video/. Distinct from the encode mode's in-memory
// manifest so the two can never be confused.
const copyPlaylistName = "copy.m3u8"

// sourceRef returns the ffmpeg/ffprobe input: the remote URL when set, else the
// local path. Used everywhere a `-i` argument or a probe target is needed so
// the local-file and debrid-URL paths share one code path.
func (cfg HLSSessionConfig) sourceRef() string {
	if cfg.SourceURL != "" {
		return cfg.SourceURL
	}
	return cfg.SourcePath
}

// burnSubtitleIndexOrNone resolves the optional burn-in subtitle pointer to the
// int sentinel the cache key and filtergraph use: nil → -1 ("no burn").
func (cfg HLSSessionConfig) burnSubtitleIndexOrNone() int {
	if cfg.BurnSubtitleIndex == nil {
		return -1
	}
	return *cfg.BurnSubtitleIndex
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
	cache          *HLSCache
	cacheKey       string
	fromCache      bool
	writerLockHeld bool

	// Live transcode telemetry (F3). ffmpeg's -stats progress line is parsed
	// in hlsStderrCapture.Write into an EWMA of speed= (×realtime) + fps=, plus
	// an input-bound hint set when the SOURCE read errors (slow/broken pull vs a
	// too-slow encode). GetTranscodeStats() snapshots this so the ready-watcher
	// can report a real measurement to the web side — letting the player name a
	// too-slow transcode honestly in ~4s instead of inferring it from stall
	// shape over 15-30s. Guarded by statsMu (the stderr goroutine writes; the
	// watcher goroutine reads).
	statsMu      sync.Mutex
	speedEWMA    float64
	fpsEWMA      float64
	speedSamples int
	warmupSeen   int // cold-start frames discarded before the EWMA is trusted
	// Walltime of the LAST source-read error ffmpeg reported. Windowed (see
	// hlsInputBoundWindow) instead of a sticky bool: with the F1 continuous
	// monitor a single transient read blip (peer drop, debrid hiccup ffmpeg
	// reconnects through) must not reclassify every sub-realtime dip as
	// "input_bound/struggling" for the rest of a multi-hour session.
	inputErrAt time.Time
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

// CloseWhere closes + removes every registered session matching pred. Used
// by the REAL-session path to reap stale prewarm encodes BEFORE its own
// StartHLSSession runs — that frees the per-key cache writer-lock, so the
// viewer's encode lands in the persistent cache instead of falling back to
// an uncached per-session tmpdir (and a SEALED prewarm survives as a cache
// HIT: closing a from-cache reader never invalidates the entry).
func (r *HLSSessionRegistry) CloseWhere(pred func(*HLSSession) bool) int {
	r.mu.Lock()
	victims := make([]*HLSSession, 0, len(r.sessions))
	for id, s := range r.sessions {
		if pred(s) {
			victims = append(victims, s)
			delete(r.sessions, id)
		}
	}
	r.mu.Unlock()
	for _, s := range victims {
		_ = s.Close()
	}
	return len(victims)
}

// IsPrewarm reports whether this session was started as a background
// cache-fill (HLSSessionConfig.Prewarm). cfg is immutable after construction.
func (s *HLSSession) IsPrewarm() bool { return s.cfg.Prewarm }

// IsVideoCopy reports whether this session serves -c:v copy (no video
// re-encode). Copy sessions emit no ffmpeg -stats telemetry, so the ready
// watcher posts a one-shot "copy" health heartbeat instead of waiting for
// speed= samples that never arrive.
func (s *HLSSession) IsVideoCopy() bool { return s.cfg.VideoCopy }

// RegisterKeep adds a session WITHOUT displacing the others — the prewarm
// path: a background cache-fill encode must not evict the viewer's live
// session (Register's eviction killed the stream being watched when the
// next-episode prewarm got claimed mid-playback). It still replaces (and
// closes) a previous session with the SAME ID. A later Register() of a real
// viewer session evicts prewarms like any other session — a completed
// (sealed) prewarm survives in the segment cache either way.
func (r *HLSSessionRegistry) RegisterKeep(s *HLSSession) {
	r.mu.Lock()
	prev := r.sessions[s.cfg.SessionID]
	r.sessions[s.cfg.SessionID] = s
	r.mu.Unlock()
	if prev != nil && prev != s {
		_ = prev.Close()
	}
}

// HasLiveEncode reports whether any registered session still has a RUNNING
// ffmpeg (encode not finished). Used to defer prewarm encodes so they never
// compete with the viewer's live transcode for the encoder.
func (r *HLSSessionRegistry) HasLiveEncode() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.sessions {
		if !s.EncodeExited() {
			return true
		}
	}
	return false
}

// Count reports how many sessions are currently registered (live or recently
// finished but not yet swept). Used by the graceful auto-upgrade gate to defer
// applying an update while the agent is actively streaming.
func (r *HLSSessionRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.sessions)
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
	if cfg.VideoCopy && cfg.Cache != nil {
		// HLS-copy never caches: re-generating costs no encode (I/O-bound), so
		// persisting segments would only burn cache budget that real transcodes
		// need. Private per-session tmpdir, deleted on Close.
		cfg.Cache = nil
	}
	if cfg.Cache != nil {
		// Debrid URL sessions key by CacheID (info_hash) so re-plays hit cache
		// despite the URL changing each resolution; local files key by path.
		if cfg.CacheID != "" {
			cacheKey = cfg.Cache.KeyForID(cfg.CacheID, cfg.Quality, cfg.AudioIndex, cfg.burnSubtitleIndexOrNone())
		} else {
			cacheKey = cfg.Cache.KeyFor(cfg.SourcePath, cfg.Quality, cfg.AudioIndex, cfg.burnSubtitleIndexOrNone())
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
	if cfg.VideoCopy {
		// Copy mode: ffmpeg owns the media playlist (segments cut at the
		// source's keyframes → durations unknown upfront, the uniform-2s
		// pre-render would lie). ServeVideoPlaylist reads it from disk.
		s.manifestVideo = ""
		s.manifestRoot = renderMasterPlaylistCopy(probe)
	} else {
		s.manifestVideo = renderVideoPlaylist(probe.DurationSec, segCount)
		s.manifestRoot = renderMasterPlaylist(probe, cfg.Quality)
	}

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

	// Resume-aware first spawn: when the session carries a StartSec (resume
	// point / position on a quality switch), launch ffmpeg already seeked at
	// the segment containing it. The web player opens playback at the same
	// position (hls.js startPosition), so segment 0 would never be requested —
	// encoding from 0 just to seek-restart milliseconds later wasted a full
	// ffmpeg spawn and doubled the resume latency. Earlier segments simply
	// don't exist on disk; ServeSegment's `idx < segStart` branch restarts the
	// encoder if the user later scrubs back before the resume point. A partial
	// encode never seals the cache (allSegmentsPresent checks 0..N), matching
	// today's post-seek behaviour.
	startIdx := 0
	if cfg.VideoCopy {
		// Copy mode always starts from 0: segment indices don't map to
		// uniform 2s slots, so a StartSec-derived index would be wrong.
		// StartSec is intentionally ignored (see buildHLSCopyArgs); the
		// player seeks to the resume point via its own startPosition once
		// the growing playlist reaches that position.
	} else if cfg.StartSec > 0 && cfg.StartSec < probe.DurationSec {
		startIdx = segmentIdxForTime(cfg.StartSec)
		if startIdx > segCount-1 {
			startIdx = segCount - 1
		}
	} else if cfg.StartSec >= probe.DurationSec && cfg.StartSec > 0 {
		// Stale resume beyond this source's duration (the file was replaced by
		// a shorter cut, or progress was saved against another release). Start
		// from the beginning instead of encoding only the final segment, which
		// would "end" the video seconds after it starts.
		log.Printf("[hls %s] startSec %.0f ≥ duration %.0f — starting from 0",
			shortHLSID(cfg.SessionID), cfg.StartSec, probe.DurationSec)
	}
	s.ffmpegSegStart = startIdx
	s.readyMax = startIdx

	// Spawn ffmpeg under a dedicated context so Close() can kill it without
	// touching the parent ctx.
	ffCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	var args []string
	if cfg.VideoCopy {
		args = buildHLSCopyArgs(cfg, probe, tmpDir)
	} else {
		args = buildHLSFFmpegArgsAt(cfg, probe, tmpDir, startIdx, segmentStartSec(startIdx))
	}
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

	// Subtitles are no longer extracted per-session: the web player fetches each
	// text track on demand as WebVTT from the /sub endpoint (subtitleHandler).
	// The old per-session extraction wrote subs/sub-N.vtt that nothing requests
	// anymore (the master playlist no longer advertises a SUBTITLES group), so
	// it was pure wasted ffmpeg work — and its Close() wait could block HLS cache
	// persistence on a slow extract. Removed.

	cachedNote := ""
	if cfg.Cache != nil {
		cachedNote = fmt.Sprintf(" (cache-miss %s)", cacheKey)
	}
	// Surface the encoder profile so a "first-start was slow" report can be
	// triaged from the agent log alone — `encoder=libx264 accel=none` means
	// the user's ffmpeg has no HW encoders compiled in, which is the most
	// common root cause (linuxbrew, default brew formula on macOS).
	encoderNote := ""
	if cfg.VideoCopy {
		encoderNote = "encoder=copy (no video re-encode)"
	} else {
		profile := ResolveEncoderProfile(cfg.Transcode.HWAccel, cfg.Transcode.Preset)
		presetNote := ""
		if profile.Preset != "" {
			presetNote = " preset=" + profile.Preset
		}
		encoderNote = fmt.Sprintf("encoder=%s accel=%s%s", profile.Codec, string(cfg.Transcode.HWAccel), presetNote)
	}
	startNote := ""
	if cfg.VideoCopy && cfg.StartSec > 0 {
		// Copy ignores StartSec on purpose (see buildHLSCopyArgs) — log the
		// requested resume point honestly so nobody reads "ffmpeg seeked".
		startNote = fmt.Sprintf(" resume=%.0fs requested (copy encodes from 0)", cfg.StartSec)
	} else if startIdx > 0 {
		startNote = fmt.Sprintf(" start=seg-%d@%.0fs", startIdx, segmentStartSec(startIdx))
	}
	log.Printf("[hls %s] started: %s, %.1fs, %d segs (quality=%s, %s)%s%s",
		shortHLSID(cfg.SessionID), cfg.logName(),
		probe.DurationSec, segCount, coalesce(cfg.Quality, "auto"),
		encoderNote, cachedNote, startNote)
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
		// `external`/`path` let the stream server attach a tokened /sub vttUrl
		// (path-addressed for sidecars, index-addressed for embedded). `path` is
		// stripped after the URL is built so the raw path isn't doubled in JSON.
		subs = append(subs, map[string]any{
			"index":    sb.Index,
			"lang":     sb.Lang,
			"codec":    sb.Codec,
			"title":    sb.Title,
			"forced":   sb.Forced,
			"text":     sb.IsTextSubtitle(),
			"external": sb.External,
			"path":     sb.Path,
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

// ReadyCount returns the session's readyMax watermark: segment idx is on disk
// iff idx < ReadyCount() AND idx >= WriterStartIdx(). For a from-zero encode
// this is simply "how many segments are on disk"; for a resume session
// (StartSec > 0) readyMax is pre-seeded to the start index, so the FIRST real
// segment has landed only once ReadyCount() > WriterStartIdx() — use that
// comparison, not `>= 1`, to flip the player's "Preparando…" UI. For
// cache-HIT sessions this is always `segmentCount` from the moment
// StartHLSSession returns.
func (s *HLSSession) ReadyCount() int {
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	return s.readyMax
}

// EncodeExited reports whether this session's ffmpeg has finished (clean or
// crashed) or never ran (cache HIT). False while an encode is producing
// segments. Used by HasLiveEncode to defer prewarm work.
func (s *HLSSession) EncodeExited() bool {
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	return s.exited
}

// WriterStartIdx returns the segment index the CURRENT ffmpeg writer started
// at: 0 for a from-the-beginning encode, the resume segment for a StartSec
// session, the seek target after a seek-restart. See ReadyCount for the
// "first segment landed" comparison.
func (s *HLSSession) WriterStartIdx() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ffmpegSegStart
}

// FromCache reports whether this session was served from the HLS cache
// (no ffmpeg subprocess spawned). Used by ready-watcher logic to short-
// circuit polling — a cache HIT is ready the moment we return.
func (s *HLSSession) FromCache() bool { return s.fromCache }

// TranscodeStats is a point-in-time snapshot of live ffmpeg progress for one
// HLS session (F3). SpeedX < 1.0 means the encode runs slower than realtime —
// the player can't sustain playback without buffering. Samples==0 means no
// -stats line has been parsed yet (the watcher keeps waiting before reporting).
type TranscodeStats struct {
	SpeedX     float64 // EWMA of ffmpeg speed= (×realtime; 1.0 = exactly realtime)
	Fps        float64 // EWMA of ffmpeg fps=
	Samples    int     // progress lines parsed so far (0 = no telemetry yet)
	InputBound bool    // source read hit I/O errors (slow/broken pull, not encode)
	FromCache  bool    // replayed from cache → no live encode, stats meaningless
}

// GetTranscodeStats returns a snapshot of the parsed ffmpeg progress EWMAs.
func (s *HLSSession) GetTranscodeStats() TranscodeStats {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	return TranscodeStats{
		SpeedX:     s.speedEWMA,
		Fps:        s.fpsEWMA,
		Samples:    s.speedSamples,
		InputBound: !s.inputErrAt.IsZero() && time.Since(s.inputErrAt) < hlsInputBoundWindow,
		FromCache:  s.fromCache,
	}
}

// hlsInputBoundWindow bounds how long a source-read error keeps classifying
// the session as input-bound. Past it, a sub-realtime encode is the encoder's
// own problem again (the transient link blip resolved or ffmpeg reconnected).
const hlsInputBoundWindow = 30 * time.Second

// hlsStatsWarmupSkip is how many leading -stats frames to discard before
// trusting the EWMA. ffmpeg's first readings reflect the pipeline filling
// (often speed=0.0x) and would otherwise drag a healthy encoder into a false
// "struggling" verdict that pauses a stream which plays fine once warmed up.
const hlsStatsWarmupSkip = 2

// recordProgress folds one parsed ffmpeg -stats sample into the session EWMAs.
// alpha=0.3 smooths the noisy per-line numbers while still tracking a sustained
// slowdown within a few samples (~2s of encoding).
func (s *HLSSession) recordProgress(speedX, fps float64) {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	// Drop the cold-start frames so a steady-state slowdown — not the encoder
	// spin-up — is what the watcher reports.
	if s.warmupSeen < hlsStatsWarmupSkip {
		s.warmupSeen++
		return
	}
	const alpha = 0.3
	if s.speedSamples == 0 {
		s.speedEWMA = speedX
		s.fpsEWMA = fps
	} else {
		s.speedEWMA = alpha*speedX + (1-alpha)*s.speedEWMA
		s.fpsEWMA = alpha*fps + (1-alpha)*s.fpsEWMA
	}
	s.speedSamples++
}

// markInputBound flags that ffmpeg reported a source-read error — the wall is
// the input pull (slow debrid link / dropped torrent peer), not the encoder.
func (s *HLSSession) markInputBound() {
	s.statsMu.Lock()
	s.inputErrAt = time.Now()
	s.statsMu.Unlock()
}

// resetTranscodeStats re-arms the cold-start warmup and drops the EWMAs +
// input-error mark. MUST be called whenever a NEW ffmpeg process starts
// inside the same session (seek restart, auto-restart supervisor): the new
// process's pipeline-fill frames read speed=0.0x, and folding them into the
// already-warmed EWMA drags a healthy 1.5x encode under the 0.75 struggling
// floor in two samples — which the F1 health monitor would then report as a
// false "struggling" (pausing the player) right at the seek the user made.
func (s *HLSSession) resetTranscodeStats() {
	s.statsMu.Lock()
	s.warmupSeen = 0
	s.speedSamples = 0 // recordProgress re-seeds the EWMA on the next sample
	s.inputErrAt = time.Time{}
	s.statsMu.Unlock()
}

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
		if exitErr == nil && s.allSegmentsPresent() {
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

	// Copy mode: no auto-restart. restartFromSegment's `-ss segmentStartSec(N)`
	// math assumes uniform 2s segments, which copy mode doesn't have — a
	// restart would corrupt the timeline. A failed copy surfaces through the
	// player's probe deadline / fallback chain instead.
	if s.cfg.VideoCopy {
		log.Printf("[hls %s] copy session failed — not restarting (player falls back)", shortHLSID(s.cfg.SessionID))
		return
	}

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
			//
			// For VideoCopy sessions, segmentCount is the encode-mode estimate
			// (ceil(dur/2s)) and is always larger than the real segment count
			// on wide-GOP sources (keyframe-cut → fewer segments). We must
			// NOT rely solely on `i == s.segmentCount-1` to detect the last
			// real segment — when exited and no successor exists the current
			// segment IS the last one, regardless of its index.
			noSuccessor := func() bool { _, e := os.Stat(next); return e != nil }
			if i == s.segmentCount-1 || (exited && noSuccessor()) {
				if !exited {
					break
				}
				highest = i + 1
				break
			}
			if noSuccessor() {
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
		// Exit when all expected segments are ready. For encode mode,
		// segmentCount is exact; for VideoCopy it's an overestimate, but the
		// `exited && noSuccessor()` branch above always marks the real last
		// segment, so highest will reach segmentCount only if the source
		// happens to have exactly that many keyframe segments — or never if
		// it has fewer. Exit also when exited and highest stopped advancing
		// (no more segments will ever appear).
		if exited && (highest >= s.segmentCount || highest == start) {
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
	if s.cfg.VideoCopy {
		s.serveCopyPlaylist(w, r)
		return
	}
	_, _ = io.WriteString(w, s.manifestVideo)
}

// serveCopyPlaylist serves ffmpeg's own media playlist (VideoCopy mode). The
// file appears within ~1 s of spawn (copy is I/O-bound) but the player's
// first fetch can race it — poll briefly instead of returning a 404 hls.js
// would surface as a manifest error. Each request re-reads the file: the
// playlist GROWS (EVENT) until ffmpeg appends ENDLIST, and players re-poll
// growing playlists by design.
func (s *HLSSession) serveCopyPlaylist(w http.ResponseWriter, r *http.Request) {
	path := filepath.Join(s.tmpDir, "video", copyPlaylistName)
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 {
			// Until ENDLIST lands a copy session is a growing EVENT playlist,
			// and some native players (iOS) treat any not-yet-ended playlist
			// like LIVE and join at the live edge instead of position 0.
			// EXT-X-START pins the start to 0 explicitly (RFC 8216 §4.3.5.2);
			// harmless once the playlist is final.
			out := data
			if !strings.Contains(string(data), "#EXT-X-START") {
				// Anchor on #EXTM3U (REQUIRED first line per RFC 8216) instead
				// of a specific VERSION value, so an ffmpeg that bumps the
				// playlist version can't silently skip the injection.
				replaced := strings.Replace(string(data),
					"#EXTM3U\n",
					"#EXTM3U\n#EXT-X-START:TIME-OFFSET=0,PRECISE=YES\n", 1)
				if replaced == string(data) {
					log.Printf("[hls %s] WARNING: EXT-X-START injection failed (no #EXTM3U header?)", shortHLSID(s.cfg.SessionID))
				}
				out = []byte(replaced)
			}
			_, _ = w.Write(out)
			return
		}
		if r.Context().Err() != nil || time.Now().After(deadline) {
			http.Error(w, "playlist not ready", http.StatusServiceUnavailable)
			return
		}
		select {
		case <-r.Context().Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
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
	// segmentCount is exact for the encode mode (uniform 2s slots) but only an
	// ESTIMATE for copy mode (cuts go at source keyframes): a short-GOP source
	// can legitimately produce more segments than the estimate, and bounding
	// would 404 the real tail. Copy trusts ffmpeg's playlist as the authority.
	if idx < 0 || (!s.cfg.VideoCopy && idx >= s.segmentCount) {
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

	// Copy mode never seek-restarts: ffmpeg outruns playback (I/O-bound), the
	// playlist only lists fully-written segments (temp_file), and segment
	// indices don't map to uniform 2s slots anyway. Just wait for the writer.
	if !s.cfg.VideoCopy && (idx >= readyMax+hlsSeekAhead || idx < segStart) {
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
	if s.cfg.VideoCopy {
		// Defensive: callers already gate on VideoCopy, but the `-ss
		// segmentStartSec(N)` math below assumes uniform 2s segments and
		// would corrupt a copy session's keyframe-cut timeline.
		return errors.New("hls: seek-restart not supported in copy mode")
	}
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
	s.resetTranscodeStats() // new ffmpeg = new cold ramp; don't poison the EWMA
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

// ---- ffmpeg argument builders ----

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
	// -stats forces ffmpeg to emit the frame=/fps=/speed= progress line to
	// stderr even at -loglevel warning; hlsStderrCapture parses it for live
	// transcode telemetry (F3) without logging it.
	args := []string{"-y", "-hide_banner", "-loglevel", "warning", "-stats"}

	// F4 — full-GPU NVENC downscale. When we're downscaling an SDR source with
	// NVENC on a host whose ffmpeg can run scale_cuda, and NO subtitle is burned
	// in, keep the decoded frame on the GPU through scale + encode (scale_cuda →
	// h264_nvenc) instead of copying every frame to the CPU for `scale=`. That
	// CPU round-trip is the wall on modest GPUs (a strong box still gains ~37%).
	// Strictly gated — the cases that need CPU frames stay on the CPU path:
	//   - HDR (the libplacebo Vulkan / zscale CPU tonemap can't consume a CUDA
	//     surface, and mixing CUDA scale with the Vulkan pass is fragile),
	//   - burn-in (the scale2ref+overlay composite runs on CPU frames),
	//   - non-NVENC encoders, and no-op when not actually downscaling.
	// Output height cap for this session — resolved once here so the F4 gate and
	// the filter chain below share ONE value (a drift between them would emit
	// scale_cuda for a height that isn't actually a downscale).
	qcap := resolveQualityCap(cfg.Quality)
	maxH := qcap.MaxHeight
	if maxH == 0 {
		maxH = cfg.Transcode.MaxHeight
	}
	useCudaScale := profile.Codec == "h264_nvenc" &&
		profile.DecodeHwAccel == "cuda" &&
		cfg.Transcode.HasScaleCuda &&
		probe.HDR == "" &&
		cfg.burnSubtitleIndexOrNone() < 0 &&
		maxH > 0 && probe.Height > maxH

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
		// F4: pin decoded frames as CUDA surfaces ONLY on the gated scale_cuda
		// path, so scale_cuda + h264_nvenc avoid the CPU copy. Off otherwise —
		// the CPU filter chain can't consume CUDA surfaces.
		if useCudaScale {
			args = append(args, "-hwaccel_output_format", "cuda")
		}
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

	// Burn a bitmap subtitle (PGS/DVB) into the video when requested. Validate
	// the index points at a real bitmap track in range — text subs are served as
	// separate WebVTT and never burned, and a stale/out-of-range index falls
	// back to a clean encode rather than failing the session.
	burnIdx := -1
	if reqBurn := cfg.burnSubtitleIndexOrNone(); reqBurn >= 0 {
		if reqBurn < len(probe.SubtitleTracks) &&
			!probe.SubtitleTracks[reqBurn].IsTextSubtitle() {
			burnIdx = reqBurn
		} else {
			log.Printf("[hls %s] burn subtitle %d ignored — not a bitmap track in range",
				shortHLSID(cfg.SessionID), reqBurn)
		}
	}

	// Map video + selected audio. With burn-in the video comes from the
	// filter_complex graph ([vout], built below); otherwise map the source video
	// stream directly. ffmpeg resolves the [vout] label from -filter_complex
	// regardless of argv order, so mapping it here (before audio) keeps video as
	// output stream 0.
	if burnIdx >= 0 {
		args = append(args, "-map", "[vout]")
	} else {
		args = append(args, "-map", "0:v:0")
	}
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
	// Clamp to an audio track that actually exists. The web persists the chosen
	// audioIndex globally, so a value from a multi-track file can arrive for a
	// file with fewer tracks; `-map 0:a:N?` would then match nothing and the
	// optional `?` silently yields a VIDEO-ONLY stream (no sound — 2026-06-03,
	// Wistoria S02E08 had one audio track but the session carried audioIndex=2).
	// Fall back to the first track so audio is never silently dropped.
	if n := len(probe.AudioTracks); n > 0 && audioIdx >= n {
		log.Printf("[hls %s] audioIndex %d out of range (%d audio track(s)) — using 0:a:0",
			shortHLSID(cfg.SessionID), audioIdx, n)
		audioIdx = 0
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
		// -bf 0 (no B-frames) + -sc_threshold 0 (no scene-cut keyframes): both
		// remove the timestamp irregularities that make ffmpeg's HLS muxer emit
		// "Packet duration is out of range" on slightly-VFR / B-frame sources and
		// produce uneven segment lengths the player stutters on. Keyframe cadence
		// is driven by -force_key_frames below, so disabling scene-cut keeps every
		// segment exactly hls_time long.
		args = append(args, "-preset", profile.Preset, "-threads", "0", "-bf", "0", "-sc_threshold", "0")
	case "h264_nvenc":
		// p3 + vbr keeps NVENC fast (~1.5 s seg-0) without the segmentation
		// breakage `-tune ll` introduced in 0.9.9: with -tune=ll the NVENC
		// rate control emits long IDR-less GOPs that ignore -force_key_frames,
		// so ffmpeg's HLS muxer never closes seg-0 and the player stalls at
		// "preparando sesión" until the 60 s mark-ready timeout. Verified on
		// ffmpeg 6.1.1 + driver 580 / RTX-class GPUs: dropping -tune ll
		// restores per-segment cuts at 27x real-time vs 28x with -tune ll.
		// -bf 0 + -no-scenecut: same rationale as libx264 (NVENC's own flag for
		// scene-cut). No B-frame reorder → monotonic DTS → uniform segments, no
		// "Packet duration is out of range" flood. Safe with -force_key_frames
		// (unlike -tune ll, which broke per-segment cuts — see note above).
		// -forced-idr 1 is LOAD-BEARING: NVENC emits -force_key_frames frames
		// as plain (non-IDR) I-frames on current ffmpeg/driver combos, the HLS
		// muxer only cuts on IDR, and every segment silently stretches to the
		// default GOP (250 frames ≈ 10.4 s @24fps) while the server-rendered
		// playlist still promises hlsSegmentDuration. The PTS↔playlist mismatch
		// breaks seeks and desyncs subtitles (measured 2026-06-10: 3 segments
		// per 30 s instead of 15; with -forced-idr exactly 15).
		args = append(args, "-preset", profile.Preset, "-rc", "vbr", "-bf", "0", "-no-scenecut", "1", "-forced-idr", "1")
	case "h264_qsv":
		// veryfast is the fastest realistic QSV preset; medium was too
		// conservative for first-start. look_ahead=0 keeps the encoder
		// truly low-latency (no rate-control look-ahead window).
		// -forced_idr: same non-IDR forced-keyframe failure mode as NVENC (see
		// above) — QSV's AVOption spells it with an underscore.
		args = append(args, "-preset", profile.Preset, "-look_ahead", "0", "-forced_idr", "1")
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
	// Derive H.264 level from the actual output FRAME (width × height), not just
	// height. A fixed "4.0" caps the encoder at 1080p; deriving by height alone
	// still under-levels anamorphic content — a 2.39:1 source scaled to 1080
	// height is ~2586×1080 = 11016 MBs, busting level 4.1's 8192-MB cap, which
	// fails the encode ("Invalid Level" on nvenc, "frame MB size > level limit"
	// on libx264) and stalls the session. The output height matches qcap.MaxHeight
	// when the source is downscaled, otherwise probe.Height; the output width is
	// the source width scaled by the same factor (the filter chain preserves AR).
	// qcap + maxH were resolved once at the top (shared with the F4 gate).
	outputHeight := qcap.MaxHeight
	if outputHeight == 0 {
		outputHeight = cfg.Transcode.MaxHeight
	}
	if outputHeight == 0 || (probe.Height > 0 && probe.Height < outputHeight) {
		outputHeight = probe.Height
	}
	outputWidth := probe.Width
	if probe.Height > 0 && outputHeight != probe.Height {
		outputWidth = int(math.Round(float64(probe.Width) * float64(outputHeight) / float64(probe.Height)))
	}
	args = append(args, "-profile:v", "main", "-level:v", H264LevelForFrame(outputWidth, outputHeight))

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
	// Rate control: capped constant-quality where the encoder supports it well
	// (libx264 CRF, NVENC CQ), plain CBR-ish elsewhere. Constant quality is the
	// on-the-fly analogue of per-title encoding: easy scenes (dialogue, anime
	// flats) emit FAR fewer bits than the fixed target — which is what keeps a
	// funnel/LTE link from stalling — while complex scenes can still use up to
	// `-maxrate` (the same ceiling as before, so worst-case quality and the
	// level-derived VBV pair are unchanged). `-bufsize 2×maxrate` gives the VBV
	// a standard one-segment window to absorb spikes; the old 1× window forced
	// the encoder to flatline at the cap. CPB stays far below every H.264
	// level's limit (level 3.1 allows 14 Mbps CPB vs our 3M at 480p).
	switch codec {
	case "libx264":
		// Capped CRF: no -b:v (CRF drives quality), -maxrate/-bufsize cap it.
		args = append(args, "-crf", "23", "-maxrate", bitrate, "-bufsize", doubleBitrate(bitrate))
	case "h264_nvenc":
		// NVENC constant-quality VBR: -cq targets quality, -b:v 0 disables the
		// default 2M average-bitrate target that would otherwise fight it.
		args = append(args, "-cq", "23", "-b:v", "0", "-maxrate", bitrate, "-bufsize", doubleBitrate(bitrate))
	default:
		// QSV / VideoToolbox / VAAPI: keep the proven fixed-bitrate triple —
		// their constant-quality knobs (ICQ, -q:v) have vendor-specific gotchas
		// (VideoToolbox ignores -q:v when -b:v is set; QSV ICQ conflicts with
		// look_ahead=0) and we can't regression-test them here.
		args = append(args, "-b:v", bitrate, "-maxrate", bitrate, "-bufsize", bitrate)
	}

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
	// (maxH was resolved once at the top, shared with the F4 cuda-scale gate.)
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
	// HDR→SDR tonemap, after the scale (downscale-first = fewer pixels to map).
	// Prefer libplacebo (GPU, ONE pass — it also emits the BT.709 colorspace +
	// 8-bit format, so it REPLACES the format/setparams tail); else the zscale
	// CPU chain; else play untonemapped (desaturated, last resort). Skip
	// libplacebo on VAAPI: its Vulkan surface flow doesn't compose with our
	// nv12+hwupload path, so VAAPI keeps the zscale-or-none behaviour.
	//
	// Gate on a real HW encoder (HWAccel != none): only then is the Vulkan
	// device a genuine GPU. A software-only host with mesa would expose lavapipe
	// (CPU Vulkan), which the functional probe accepts but whose tonemap is
	// SLOWER than the zscale CPU chain — so on those hosts libplacebo would be a
	// regression. No HW encoder ⇒ stay on zscale.
	useLibplacebo := probe.HDR != "" && cfg.Transcode.HasLibplacebo &&
		codec != "h264_vaapi" && cfg.Transcode.HWAccel != HWAccelNone
	tonemap := ""
	if probe.HDR != "" && cfg.Transcode.TonemapHDR && !useLibplacebo {
		tonemap = hdrTonemapChain
	}
	// videoTail = everything after the scale: either libplacebo (tonemap +
	// colorspace + format in one) or the (optional zscale) tonemap then the
	// format + color-metadata tail. No leading comma — the scale chain ends in one.
	videoTail := tonemap + "format=" + pixFormat + colorTail
	if useLibplacebo {
		videoTail = libplaceboTonemapFilter
	}
	// Core video chain (scale + tonemap/format tail), WITHOUT the optional
	// hwUploadTail — that has to run last, after any subtitle overlay, so it's
	// appended separately below.
	var vchain string
	switch {
	case useCudaScale:
		// F4: scale on the CUDA surface and hand h264_nvenc a yuv420p CUDA frame
		// directly — no CPU `format`/`setparams` tail (the frame never leaves the
		// GPU; nvenc records BT.709 SDR metadata from the source). scale_cuda's
		// `-2` already yields an even width, so the second even-rounding pass the
		// CPU path needs is unnecessary. useCudaScale already implies a real
		// downscale (probe.Height > cudaCap) on an SDR, non-burn-in NVENC source.
		vchain = fmt.Sprintf("scale_cuda=-2:%d:format=yuv420p", maxH)
	case maxH > 0 && probe.Height > maxH:
		vchain = fmt.Sprintf(
			"scale=-2:%d:force_original_aspect_ratio=decrease,scale=trunc(iw/2)*2:trunc(ih/2)*2,%s",
			maxH, videoTail,
		)
	default:
		vchain = fmt.Sprintf(
			"scale=trunc(iw/2)*2:trunc(ih/2)*2,%s",
			videoTail,
		)
	}
	if burnIdx >= 0 {
		// Burn-in: process the video to its final size + SDR colorspace FIRST,
		// then composite the subtitle. Overlaying SDR PGS/DVB graphics onto a
		// still-HDR (PQ) frame and tonemapping afterwards would crush the
		// subtitle brightness, so the overlay must come after the tonemap. The
		// subtitle canvas is scaled to the processed frame via scale2ref so a
		// PGS/DVB stream authored at any resolution lines up. hwUploadTail
		// (VAAPI) runs last, on the composited frame.
		filterComplex := fmt.Sprintf(
			"[0:v:0]%s[base];[0:s:%d][base]scale2ref[sub][base2];[base2][sub]overlay%s[vout]",
			vchain, burnIdx, hwUploadTail,
		)
		args = append(args, "-filter_complex", filterComplex)
	} else {
		args = append(args, "-vf", vchain+hwUploadTail)
	}

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

	// Force constant frame rate. Many MKV rips are slightly variable-frame-rate
	// (or carry irregular PTS); muxed to fMP4 that produces non-monotonic packet
	// durations ("Packet duration is out of range") and uneven segment lengths
	// the player stutters on. CFR resamples to a steady cadence → uniform
	// segments. Near-CFR sources (23.976/24/25) are essentially untouched.
	args = append(args, "-fps_mode", "cfr")

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
// buildHLSCopyArgs builds the ffmpeg invocation for VideoCopy mode: video
// stream copied bit-exact (`-c:v copy`, the segments cut at the source's own
// keyframes), audio copied when already AAC or re-encoded to AAC 192k
// otherwise, muxed to fMP4 HLS with ffmpeg writing its OWN media playlist
// (EVENT while running, ENDLIST on completion) with byte-exact EXTINF
// durations. Validated empirically on the incident source (HEVC Main10 +
// EAC3 MKV): seg-0 TTFB ~510 ms, valid hvc1+mp4a stream.
//
// Deliberate differences from the encode path:
//   - no encoder/preset/bitrate/keyframe flags (nothing is encoded);
//   - `+temp_file` so segments land atomically (write .tmp → rename) and a
//     listed segment is always complete on disk;
//   - playlist type EVENT: the timeline grows as ffmpeg outruns playback
//     (I/O-bound) and players treat it as live-DVR until ENDLIST.
func buildHLSCopyArgs(cfg HLSSessionConfig, probe *StreamProbe, tmpDir string) []string {
	args := []string{"-y", "-hide_banner", "-loglevel", "warning", "-stats"}

	// StartSec is INTENTIONALLY ignored in copy mode: an EVENT playlist whose
	// entries start at position 0 while the fragments carry an offset tfdt
	// (-ss + -output_ts_offset) is exactly the shape iOS's native HLS parser
	// chokes on (observed 2026-06-10: resume at 368s → player error + session
	// re-bootstrap loop on iPhone). Copy always produces from 0 with true
	// absolute PTS — it outruns playback at I/O speed, so the resume point
	// appears in the growing timeline within seconds and the player's own
	// startPosition seek lands normally.

	if cfg.SourceURL != "" {
		args = append(args,
			"-reconnect", "1",
			"-reconnect_streamed", "1",
			"-reconnect_delay_max", "5",
			"-rw_timeout", "30000000",
		)
	}
	args = append(args, "-i", cfg.sourceRef())

	// Map video + selected audio (same clamping rules as the encode path).
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
	if n := len(probe.AudioTracks); n > 0 && audioIdx >= n {
		log.Printf("[hls %s] audioIndex %d out of range (%d audio track(s)) — using 0:a:0",
			shortHLSID(cfg.SessionID), audioIdx, n)
		audioIdx = 0
	}
	args = append(args, "-map", fmt.Sprintf("0:a:%d?", audioIdx))

	// Video: bit-exact copy. HEVC needs the hvc1 tag or Safari/Apple refuses
	// the track (mkv extracts default to hev1).
	args = append(args, "-c:v", "copy")
	if strings.EqualFold(probe.VideoCodec, "hevc") {
		args = append(args, "-tag:v", "hvc1")
	}

	// Audio: copy ONLY when the selected track is AAC with ≤2 channels —
	// WebKit/Apple HLS rejects multichannel AAC at the first media segment
	// (observed via the Safari access log: master → index → init → seg-0
	// fetched twice, then silence — every 5.1 movie failed on iPhone while
	// stereo-AAC episodes played). Anything else (non-AAC, or AAC 5.1+) is
	// re-encoded mirroring the encode path exactly: AAC stereo 48k. The
	// original multichannel track stays intact for external players.
	audioCodec := probe.AudioCodec
	audioChannels := 0
	if audioIdx < len(probe.AudioTracks) {
		audioCodec = probe.AudioTracks[audioIdx].Codec
		audioChannels = probe.AudioTracks[audioIdx].Channels
	}
	if strings.EqualFold(audioCodec, "aac") && audioChannels > 0 && audioChannels <= 2 {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, "-c:a", "aac", "-b:a", "192k", "-ar", "48000", "-ac", "2")
	}

	args = append(args,
		"-f", "hls",
		"-hls_time", strconv.Itoa(hlsSegmentDuration),
		"-hls_playlist_type", "event",
		"-hls_segment_type", "fmp4",
		"-hls_list_size", "0",
		"-hls_flags", "independent_segments+temp_file",
		"-hls_fmp4_init_filename", "init.mp4",
		"-hls_segment_filename", filepath.Join(tmpDir, "video", "seg-%d.m4s"),
		filepath.Join(tmpDir, "video", copyPlaylistName),
	)
	return args
}

// renderMasterPlaylistCopy builds the master playlist for VideoCopy mode.
// Unlike the encode master it deliberately OMITS the CODECS attribute: the
// stream carries the source's codec verbatim (hvc1/avc1/av01 at whatever
// profile/level the file has) and a wrong hardcoded string makes iOS reject
// the variant outright, while omission is legal and universally tolerated.
// Resolution/bandwidth are the source's real values (best-effort).
func renderMasterPlaylistCopy(probe *StreamProbe) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:7\n")
	// BANDWIDTH is advisory (single variant, no ABR) — a height-based
	// estimate of typical source bitrates is plenty.
	bw := 8_000_000
	switch {
	case probe.Height >= 2000:
		bw = 25_000_000
	case probe.Height >= 1000:
		bw = 10_000_000
	case probe.Height >= 700:
		bw = 5_000_000
	}
	b.WriteString(fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%dx%d\n", bw, probe.Width, probe.Height))
	b.WriteString("video/index.m3u8\n")
	return b.String()
}

func renderMasterPlaylist(probe *StreamProbe, qualityLabel string) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:7\n")

	// Subtitles are no longer embedded as HLS renditions. The web player attaches
	// every TEXT subtitle as an external <track> served on demand by the /sub
	// endpoint (subtitleHandler) — ONE source for direct-play AND HLS that works
	// under native playback and hls.js alike. Embedding them here too would
	// double the captions menu under hls.js, and the native-HLS path (Chrome's
	// "maybe" canPlayType) never surfaced in-manifest SUBTITLES renditions
	// anyway, which is what made subtitles inconsistent. Bitmap subs (PGS/DVB)
	// remain burn-in (no WebVTT form).

	// Video variant. Bandwidth + resolution are best-effort estimates from probe.
	bw := bitrateForQuality(qualityLabel)
	w, h := scaledDimensions(probe.Width, probe.Height, qualityHeight(qualityLabel))
	codecs := `avc1.4D4028,mp4a.40.2`
	b.WriteString(fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%dx%d,CODECS=%q\n", bw, w, h, codecs))
	b.WriteString("video/index.m3u8\n")
	return b.String()
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

// ffmpeg -stats progress lines look like:
//
//	frame=  123 fps= 30 q=28.0 size=  456kB time=00:00:08.00 speed=1.05x
//
// emitted with a trailing \r (overwrite-in-place), once per ~0.5s. We parse
// speed=/fps= out of them for live transcode telemetry (F3) and DON'T log them
// (one per 0.5s would drown the daemon log) — only \n-terminated warning/error
// lines reach log.Printf below.
var (
	reFFmpegSpeed = regexp.MustCompile(`speed=\s*([0-9.]+)x`)
	reFFmpegFps   = regexp.MustCompile(`fps=\s*([0-9.]+)`)
)

func parseFFmpegProgress(line string) (speedX, fps float64, ok bool) {
	m := reFFmpegSpeed.FindStringSubmatch(line)
	if m == nil {
		return 0, 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, 0, false
	}
	if fm := reFFmpegFps.FindStringSubmatch(line); fm != nil {
		fps, _ = strconv.ParseFloat(fm[1], 64)
	}
	return v, fps, true
}

// isInputBoundLine spots ffmpeg stderr that means the SOURCE read failed (slow
// debrid link, dropped torrent peer, network timeout) rather than the encoder
// being too slow — so the player names the bottleneck as the link, not the GPU.
func isInputBoundLine(line string) bool {
	l := strings.ToLower(line)
	return strings.Contains(l, "i/o error") ||
		strings.Contains(l, "connection reset") ||
		strings.Contains(l, "rw_timeout") ||
		strings.Contains(l, "error in the pull function") ||
		strings.Contains(l, "connection timed out")
}

func (c *hlsStderrCapture) Write(p []byte) (int, error) {
	// If the incoming chunk alone exceeds the cap (very long unterminated
	// line), drop the buffered prefix AND truncate p so a single multi-MB
	// write can't grow memory.
	if len(p) > maxStderrBuf {
		c.buf.Reset()
		p = p[len(p)-maxStderrBuf:]
	} else if c.buf.Len()+len(p) > maxStderrBuf {
		// Drop the unterminated partial line; we'll resync on the next \r/\n.
		c.buf.Reset()
	}
	c.buf.Write(p)
	// Frame on \r OR \n: ffmpeg's progress line is \r-terminated, warnings are
	// \n-terminated. Parsing progress per-frame keeps the EWMA fresh; logging
	// only the \n lines keeps the log readable.
	for {
		s := c.buf.String()
		idx := strings.IndexAny(s, "\r\n")
		if idx < 0 {
			break
		}
		line := strings.TrimSpace(s[:idx])
		c.buf.Reset()
		c.buf.WriteString(s[idx+1:])
		if line == "" {
			continue
		}
		if speedX, fps, ok := parseFFmpegProgress(line); ok {
			c.owner.recordProgress(speedX, fps)
			continue // progress line — telemetry only, never logged
		}
		if isInputBoundLine(line) {
			c.owner.markInputBound()
		}
		log.Printf("[hls %s] ffmpeg: %s", shortHLSID(c.owner.cfg.SessionID), line)
	}
	return len(p), nil
}

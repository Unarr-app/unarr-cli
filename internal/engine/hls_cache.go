package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// HLSCache persists transcoded HLS segments per (source, quality, audio) so a
// second play of the same file at the same quality skips ffmpeg entirely.
//
// Layout on disk:
//
//	{root}/{key}/init.mp4
//	{root}/{key}/seg-0.m4s
//	{root}/{key}/seg-N.m4s
//	{root}/{key}/.complete
//
// Atomicity: the .complete marker is written only when ffmpeg exits 0 AND all
// segments are on disk. A dir without .complete is treated as a partial run —
// next session can reuse the segments already present, ffmpeg fills the gaps.
//
// Concurrency: Pin/Unpin increments a ref counter per key so the LRU sweeper
// never evicts a directory that an active session is reading from.
type HLSCache struct {
	root     string
	maxBytes int64

	mu      sync.Mutex
	refs    map[string]int
	writers map[string]bool // exclusive ffmpeg writer per key; nil entries are absent

	// Counters surfaced via Stats() — useful for /api/internal/agent/cache-stats
	// and for the sweeper's daily log line. atomic so RecordHit/RecordMiss are
	// safe to call from any goroutine without taking the cache mutex.
	hits   atomic.Uint64
	misses atomic.Uint64
}

const (
	hlsCacheCompleteMarker = ".complete"
	// hlsCacheMinBudgetGB clamps absurd / zero / negative SizeGB values to
	// a sane floor. NOT a guarantee that any single encode fits — a long
	// 4K HEVC re-encode can exceed it. Operators should set size_gb based
	// on their actual workload.
	hlsCacheMinBudgetGB = 1
	// hlsCacheStartupOrphanAge: directories without .complete older than
	// this are removed on cache startup. Long enough that a daemon crash
	// during an in-progress encode (which legitimately leaves a partial
	// dir) doesn't get nuked too aggressively if the daemon restarts fast.
	hlsCacheStartupOrphanAge = 10 * time.Minute
)

// NewHLSCache creates the cache rooted at the given dir with a size budget in
// gigabytes. A budget < hlsCacheMinBudgetGB is clamped up so a single play
// doesn't get instantly evicted mid-stream.
func NewHLSCache(root string, sizeGB int) (*HLSCache, error) {
	if root == "" {
		return nil, errors.New("hls_cache: empty root")
	}
	if sizeGB < hlsCacheMinBudgetGB {
		sizeGB = hlsCacheMinBudgetGB
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("hls_cache: mkdir root: %w", err)
	}
	c := &HLSCache{
		root:     root,
		maxBytes: int64(sizeGB) * 1024 * 1024 * 1024,
		refs:     make(map[string]int),
		writers:  make(map[string]bool),
	}
	// Reap dirs left over from a crashed encode. A dir without .complete that
	// hasn't been touched recently was almost certainly orphaned by an
	// ungraceful daemon exit — keeping it just feeds the unbounded growth
	// pattern the hourly LRU is too slow to contain.
	if removed, err := c.cleanStartupOrphans(); err != nil {
		log.Printf("[hls_cache] startup orphan cleanup: %v", err)
	} else if removed > 0 {
		log.Printf("[hls_cache] startup: removed %d orphan dir(s) without .complete", removed)
	}
	return c, nil
}

// cleanStartupOrphans removes cache subdirectories that lack a .complete
// marker AND haven't been modified within hlsCacheStartupOrphanAge. Called
// once at construction. Safe at startup because no sessions are active yet,
// so Pin can't race with us.
func (c *HLSCache) cleanStartupOrphans() (int, error) {
	entries, err := os.ReadDir(c.root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := time.Now().Add(-hlsCacheStartupOrphanAge)
	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(c.root, e.Name())
		if _, err := os.Stat(filepath.Join(dir, hlsCacheCompleteMarker)); err == nil {
			continue // sealed, keep
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue // too recent — might be a daemon that just restarted mid-encode
		}
		if err := os.RemoveAll(dir); err == nil {
			removed++
		}
	}
	return removed, nil
}

// TryAcquireWriter attempts to claim exclusive ffmpeg-write access to a key.
// Returns true on success — the caller is then responsible for ReleaseWriter
// when ffmpeg exits / fails. Returns false if another session is already
// writing this key, in which case the caller must fall back to a private
// per-session tmpdir (no caching for that session).
func (c *HLSCache) TryAcquireWriter(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writers[key] {
		return false
	}
	c.writers[key] = true
	return true
}

// ReleaseWriter releases the writer claim acquired via TryAcquireWriter.
// Idempotent on unknown keys.
func (c *HLSCache) ReleaseWriter(key string) {
	c.mu.Lock()
	delete(c.writers, key)
	c.mu.Unlock()
}

// KeyFor derives a stable cache key for (source, quality, audioIndex). Using
// the absolute source path means renaming a file invalidates the cache, which
// is correct — segment content is tied to the encoded source.
func (c *HLSCache) KeyFor(sourcePath, quality string, audioIndex int) string {
	abs, err := filepath.Abs(sourcePath)
	if err != nil {
		abs = sourcePath
	}
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d", abs, quality, audioIndex)))
	return hex.EncodeToString(h[:8]) // 16 hex chars — collision-safe enough for per-host cache
}

// DirFor returns the on-disk directory for a cache key. Caller is responsible
// for creating it.
func (c *HLSCache) DirFor(key string) string {
	return filepath.Join(c.root, key)
}

// HasComplete returns true when the .complete marker is present, meaning the
// directory holds a full set of segments from a successful encode.
func (c *HLSCache) HasComplete(key string) bool {
	if _, err := os.Stat(filepath.Join(c.DirFor(key), hlsCacheCompleteMarker)); err == nil {
		return true
	}
	return false
}

// MarkComplete writes the .complete marker. Call only after verifying ffmpeg
// exited cleanly AND every expected segment is on disk. The dir must already
// exist — StartHLSSession created it on the writer path.
func (c *HLSCache) MarkComplete(key string) error {
	return os.WriteFile(filepath.Join(c.DirFor(key), hlsCacheCompleteMarker), nil, 0o644)
}

// RecordHit increments the hit counter; called by StartHLSSession on a
// cache-HIT path.
func (c *HLSCache) RecordHit() { c.hits.Add(1) }

// RecordMiss increments the miss counter; called when a session has to
// encode from scratch (or fails an integrity check on a stale HIT).
func (c *HLSCache) RecordMiss() { c.misses.Add(1) }

// CacheStats is a snapshot of the cache's runtime counters + on-disk size.
// The size fields are best-effort (computed via dirSize) so callers paying
// for them should cache the result, not poll in a hot loop.
type CacheStats struct {
	Hits       uint64
	Misses     uint64
	EntryCount int
	TotalBytes int64
}

// Stats returns a snapshot of the cache counters and size. Walks the root
// to total disk usage — O(N segments). Call at most every few minutes.
func (c *HLSCache) Stats() CacheStats {
	s := CacheStats{
		Hits:   c.hits.Load(),
		Misses: c.misses.Load(),
	}
	entries, err := os.ReadDir(c.root)
	if err != nil {
		return s
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		size, err := dirSize(filepath.Join(c.root, e.Name()))
		if err != nil {
			continue
		}
		s.EntryCount++
		s.TotalBytes += size
	}
	return s
}

// hitRatePercent returns the current hit/(hit+miss) percentage rounded to
// the nearest int; 0 when no calls have been recorded.
func (c *HLSCache) hitRatePercent() int {
	h := c.hits.Load()
	m := c.misses.Load()
	total := h + m
	if total == 0 {
		return 0
	}
	return int((h*100 + total/2) / total)
}

// VerifyComplete checks that the .complete marker is present AND the
// essential files (init.mp4 + last segment) exist with non-zero size. A
// dir that passes HasComplete but fails VerifyComplete is treated as
// corrupted — typically external `rm` or a partial-disk-failure scenario.
// When it returns false, callers should Invalidate and re-encode.
func (c *HLSCache) VerifyComplete(key string, segmentCount int) bool {
	if !c.HasComplete(key) {
		return false
	}
	dir := c.DirFor(key)
	if fi, err := os.Stat(filepath.Join(dir, "video", "init.mp4")); err != nil || fi.Size() == 0 {
		return false
	}
	if segmentCount > 0 {
		lastSeg := filepath.Join(dir, "video", fmt.Sprintf("seg-%d.m4s", segmentCount-1))
		if fi, err := os.Stat(lastSeg); err != nil || fi.Size() == 0 {
			return false
		}
	}
	return true
}

// Pin increments the ref counter for a key. The sweeper checks this before
// evicting, so a pinned dir is safe even if its mtime is old.
func (c *HLSCache) Pin(key string) {
	c.mu.Lock()
	c.refs[key]++
	c.mu.Unlock()
}

// Unpin decrements; safe to call on unknown keys (no-op).
func (c *HLSCache) Unpin(key string) {
	c.mu.Lock()
	if c.refs[key] > 0 {
		c.refs[key]--
		if c.refs[key] == 0 {
			delete(c.refs, key)
		}
	}
	c.mu.Unlock()
}

func (c *HLSCache) isPinned(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refs[key] > 0
}

// Touch updates the directory mtime so LRU picks fresher entries as recently
// used. Called when a session starts reading from a cached dir.
func (c *HLSCache) Touch(key string) error {
	dir := c.DirFor(key)
	now := time.Now()
	return os.Chtimes(dir, now, now)
}

// Sweep enforces the size budget by deleting the least-recently-used cache
// dirs (ignoring pinned ones) until the total size is at or below maxBytes.
// Returns the number of bytes freed.
func (c *HLSCache) Sweep() (int64, error) {
	entries, err := os.ReadDir(c.root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("hls_cache: read root: %w", err)
	}

	type item struct {
		key   string
		path  string
		size  int64
		mtime time.Time
	}
	items := make([]item, 0, len(entries))
	var total, pinned int64
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		key := e.Name()
		path := filepath.Join(c.root, key)
		size, err := dirSize(path)
		if err != nil {
			continue
		}
		items = append(items, item{key: key, path: path, size: size, mtime: info.ModTime()})
		total += size
		if c.isPinned(key) {
			pinned += size
		}
	}

	if total <= c.maxBytes {
		return 0, nil
	}
	if pinned >= c.maxBytes {
		// Every pinned byte already exceeds the budget — even evicting
		// every unpinned dir won't bring us under. Warn loudly so the
		// operator knows to bump size_gb (or kill the long-running session).
		log.Printf("[hls_cache] warn: pinned bytes (%.1f MB) exceed budget (%.1f MB) — cannot enforce limit until sessions release",
			float64(pinned)/(1024*1024), float64(c.maxBytes)/(1024*1024))
		return 0, nil
	}

	// Oldest first.
	sort.Slice(items, func(i, j int) bool {
		return items[i].mtime.Before(items[j].mtime)
	})

	var freed int64
	for _, it := range items {
		if total-freed <= c.maxBytes {
			break
		}
		if c.isPinned(it.key) {
			continue
		}
		if err := os.RemoveAll(it.path); err != nil {
			log.Printf("[hls_cache] evict %s failed: %v", it.key, err)
			continue
		}
		log.Printf("[hls_cache] evicted %s (%.1f MB, age %s)",
			it.key, float64(it.size)/(1024*1024), time.Since(it.mtime).Round(time.Second))
		freed += it.size
	}
	return freed, nil
}

// StartSweeper kicks off the LRU sweeper goroutine. Cancels on ctx done.
// In addition to enforcing the size budget, logs a daily summary of hit-rate
// + disk usage so operators can see the cache's value at a glance.
func (c *HLSCache) StartSweeper(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		statsTick := time.NewTicker(24 * time.Hour)
		defer statsTick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := c.Sweep(); err != nil {
					log.Printf("[hls_cache] sweep error: %v", err)
				}
			case <-statsTick.C:
				s := c.Stats()
				log.Printf("[hls_cache] day-stats: hits=%d misses=%d ratio=%d%% entries=%d size=%.1fMB",
					s.Hits, s.Misses, c.hitRatePercent(), s.EntryCount,
					float64(s.TotalBytes)/(1024*1024))
			}
		}
	}()
}

// Invalidate removes a cache entry — used when ffmpeg fails to encode the
// source so we don't reuse a half-written dir next time.
func (c *HLSCache) Invalidate(key string) error {
	return os.RemoveAll(c.DirFor(key))
}


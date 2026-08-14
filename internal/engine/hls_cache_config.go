package engine

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// Config-change invalidation.
//
// A cached segment is only interchangeable with a fresh encode when both were
// produced under the same encoder settings. The cache key carries an encode
// fingerprint (see TranscodeRuntime.EncodeFingerprint), so after a config
// change new sessions address new directories and never splice old segments
// into a new encode.
//
// That leaves the old directories on disk: correct, but dead. They can never
// be HIT again — no future session will compute their key — yet they hold the
// whole budget until the LRU sweeper happens to evict them, which on a large
// budget can be never. Worse, they are the entries most likely to be damaged,
// because a config change is what a user reaches for when playback is already
// misbehaving.
//
// So the cache records the fingerprint it was built under and drops everything
// when it changes. The cost is bounded and understood — re-encoding on next
// play, exactly what a config change implies — and it makes "change the
// settings" a real remedy rather than a step that leaves the bad entries in
// place.

// hlsCacheStampFile holds the encode fingerprint the cache directory's
// contents were produced under. Kept at the cache root, beside the per-key
// directories.
const hlsCacheStampFile = ".encode-stamp"

type hlsCacheStamp struct {
	// Fingerprint is TranscodeRuntime.EncodeFingerprint at the time the cache
	// was last built. Stored rather than hashed so an operator reading the
	// file can see which setting changed.
	Fingerprint string `json:"fingerprint"`
}

// ReconcileEncodeConfig drops every cached entry when the encoder settings
// differ from the ones the cache was built under, and records the current
// settings. Returns how many entries were removed.
//
// Called once at daemon start, before any session can Pin a key: eviction
// here cannot race a reader. A cache with no stamp (first run after upgrade)
// is stamped without being cleared — its entries were built by the same
// binary under the same settings; discarding them would be a pointless
// re-encode of everything on one upgrade.
func (c *HLSCache) ReconcileEncodeConfig(fingerprint string) (int, error) {
	stampPath := filepath.Join(c.root, hlsCacheStampFile)
	prev, err := readHLSCacheStamp(stampPath)
	if err != nil {
		return 0, err
	}

	// Unstamped cache, or settings unchanged: keep what is there.
	if prev == "" || prev == fingerprint {
		return 0, writeHLSCacheStamp(stampPath, fingerprint)
	}

	removed, err := c.removeAllEntries()
	if err != nil {
		return removed, err
	}
	log.Printf("[hls_cache] transcode settings changed (%s -> %s) - dropped %d cached encode(s); "+
		"the next play of each title re-encodes", prev, fingerprint, removed)
	return removed, writeHLSCacheStamp(stampPath, fingerprint)
}

// removeAllEntries deletes every per-key directory under the cache root,
// leaving the root and its stamp in place.
func (c *HLSCache) removeAllEntries() (int, error) {
	entries, err := os.ReadDir(c.root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("hls_cache: read root: %w", err)
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if err := os.RemoveAll(filepath.Join(c.root, e.Name())); err != nil {
			// Keep going: one undeletable entry (a file held open on Windows,
			// a permissions oddity) should not leave the rest of an
			// incompatible cache in place.
			log.Printf("[hls_cache] drop %s: %v", e.Name(), err)
			continue
		}
		removed++
	}
	return removed, nil
}

// readHLSCacheStamp returns the recorded fingerprint, or "" when the cache has
// never been stamped. An unreadable or malformed stamp is treated as absent:
// the file is a cache-validity hint, and refusing to start the daemon over it
// would be a worse failure than one extra re-encode.
func readHLSCacheStamp(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("hls_cache: read stamp: %w", err)
	}
	var s hlsCacheStamp
	if err := json.Unmarshal(b, &s); err != nil {
		log.Printf("[hls_cache] unreadable %s (%v) - treating cache as unstamped", hlsCacheStampFile, err)
		return "", nil
	}
	return s.Fingerprint, nil
}

func writeHLSCacheStamp(path, fingerprint string) error {
	b, err := json.Marshal(hlsCacheStamp{Fingerprint: fingerprint})
	if err != nil {
		return fmt.Errorf("hls_cache: encode stamp: %w", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("hls_cache: write stamp: %w", err)
	}
	return nil
}

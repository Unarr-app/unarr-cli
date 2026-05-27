package engine

import (
	"os"
	"sync"
	"time"
)

// probeCacheTTL is how long a cached probe stays usable. The cache key
// already incorporates mtime + size, so the TTL is a defense against
// runaway memory growth from stale paths, not a freshness guarantee — a
// rename + recreate at the same inode (rare) would still be caught by the
// mtime delta.
const probeCacheTTL = 30 * time.Minute

type probeCacheEntry struct {
	probe   *StreamProbe
	expires time.Time
}

type probeCacheKey struct {
	path  string
	mtime int64 // ModTime().UnixNano()
	size  int64
}

var (
	probeCacheMu sync.RWMutex
	probeCache   = make(map[probeCacheKey]probeCacheEntry)
)

// lookupProbeCache returns the cached StreamProbe for the given path if its
// mtime + size still match the value recorded at insert time, AND the cache
// entry hasn't expired. Any stat failure / mismatch returns (nil, false) so
// the caller falls through to a fresh ffprobe run.
func lookupProbeCache(path string) (*StreamProbe, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, false
	}
	key := probeCacheKey{
		path:  path,
		mtime: fi.ModTime().UnixNano(),
		size:  fi.Size(),
	}
	probeCacheMu.RLock()
	entry, ok := probeCache[key]
	probeCacheMu.RUnlock()
	if !ok {
		return nil, false
	}
	if time.Now().After(entry.expires) {
		probeCacheMu.Lock()
		delete(probeCache, key)
		probeCacheMu.Unlock()
		return nil, false
	}
	return entry.probe, true
}

// storeProbeCache stashes a fresh probe result under the (path, mtime, size)
// key. A subsequent ffprobe-skipping HIT requires the file to still have the
// same mtime + size — anything else (re-encoded, renamed+recreated at the
// same path, truncated) misses and triggers a re-probe.
func storeProbeCache(path string, probe *StreamProbe) {
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	key := probeCacheKey{
		path:  path,
		mtime: fi.ModTime().UnixNano(),
		size:  fi.Size(),
	}
	probeCacheMu.Lock()
	probeCache[key] = probeCacheEntry{
		probe:   probe,
		expires: time.Now().Add(probeCacheTTL),
	}
	probeCacheMu.Unlock()
}

// ResetProbeCache clears the in-memory probe cache. Test-only.
func ResetProbeCache() {
	probeCacheMu.Lock()
	probeCache = make(map[probeCacheKey]probeCacheEntry)
	probeCacheMu.Unlock()
}

// ProbeCacheSize returns the number of entries currently cached. Exposed
// for diagnostics + tests.
func ProbeCacheSize() int {
	probeCacheMu.RLock()
	defer probeCacheMu.RUnlock()
	return len(probeCache)
}

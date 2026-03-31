package upgrade

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/torrentclaw/unarr/internal/config"
)

const cacheTTL = 1 * time.Hour

// versionCache is the on-disk structure for cached version checks.
type versionCache struct {
	Version   string    `json:"version"`
	CheckedAt time.Time `json:"checkedAt"`
}

// cacheFilePath returns the path to the version cache file.
func cacheFilePath() string {
	return filepath.Join(config.DataDir(), "latest-version.json")
}

// ReadCachedVersion returns the cached latest version if it's fresh (< cacheTTL).
// Returns empty string if cache is missing, stale, or corrupt.
func ReadCachedVersion() string {
	data, err := os.ReadFile(cacheFilePath())
	if err != nil {
		return ""
	}
	var c versionCache
	if json.Unmarshal(data, &c) != nil {
		return ""
	}
	if time.Since(c.CheckedAt) > cacheTTL {
		return ""
	}
	return c.Version
}

// writeCachedVersion writes the latest version to the cache file.
func writeCachedVersion(version string) {
	c := versionCache{
		Version:   version,
		CheckedAt: time.Now(),
	}
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	path := cacheFilePath()
	os.MkdirAll(filepath.Dir(path), 0o755)
	// Best-effort write — ignore errors
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	os.Rename(tmp, path)
}

// CheckLatestCached returns the latest version, using cache when fresh.
// If cache is stale, fetches from GitHub and updates the cache.
func CheckLatestCached(ctx context.Context) (version string, fromCache bool, err error) {
	if cached := ReadCachedVersion(); cached != "" {
		return cached, true, nil
	}
	v, err := fetchLatestVersion(ctx)
	if err != nil {
		return "", false, err
	}
	writeCachedVersion(v)
	return v, false, nil
}

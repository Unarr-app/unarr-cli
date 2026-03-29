package library

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/torrentclaw/torrentclaw-cli/internal/config"
)

// CachePath returns the default library cache file path.
func CachePath() string {
	return filepath.Join(config.DataDir(), "library.json")
}

// LoadCache reads the library cache from disk. Returns nil if file doesn't exist.
func LoadCache() (*LibraryCache, error) {
	return LoadCacheFrom(CachePath())
}

// LoadCacheFrom reads the library cache from a specific path.
func LoadCacheFrom(path string) (*LibraryCache, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read cache: %w", err)
	}

	var cache LibraryCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, fmt.Errorf("parse cache: %w", err)
	}

	if cache.Version != cacheVersion {
		return nil, nil // incompatible version, treat as missing
	}

	return &cache, nil
}

// SaveCache writes the library cache to disk atomically.
func SaveCache(cache *LibraryCache) error {
	return SaveCacheTo(cache, CachePath())
}

// SaveCacheTo writes the library cache to a specific path atomically.
func SaveCacheTo(cache *LibraryCache, path string) error {
	cache.Version = cacheVersion

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}

	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return fmt.Errorf("encode cache: %w", err)
	}

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return fmt.Errorf("write temp cache: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename cache: %w", err)
	}

	return nil
}

// BuildCacheIndex creates a lookup map from filePath → index for incremental scanning.
func BuildCacheIndex(cache *LibraryCache) map[string]int {
	if cache == nil {
		return nil
	}
	idx := make(map[string]int, len(cache.Items))
	for i, item := range cache.Items {
		idx[item.FilePath] = i
	}
	return idx
}

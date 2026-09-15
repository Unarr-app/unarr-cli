package stream

import (
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/sysinfo"
)

// ArticleCacheSizeEnv overrides the default size of the process-wide decoded
// article cache, in MiB (at least minArticleCacheMB). For hosts where the
// memory-derived default does not fit.
const ArticleCacheSizeEnv = "UNARR_USENET_CACHE_MB"

// minArticleCacheMB is the smallest accepted override. Streaming cannot run
// without the cache: read-ahead parts that are not stored are claimed and
// fetched again on the next Read, so a cache below the read-ahead cushion
// (ReadaheadBytes) turns playback into repeated billed downloads of the same
// articles.
const minArticleCacheMB = 8

// Bounds of the memory-derived default (see articleCacheForMemory).
const (
	minDefaultArticleCache int64 = 16 << 20
	maxDefaultArticleCache int64 = 32 << 20
	// articleCacheMemoryShare: the default is 1/128 of usable memory, within the
	// bounds — 16 MiB up to 2 GiB of RAM, 32 MiB from 4 GiB.
	articleCacheMemoryShare = 128
	maxArticleCacheMB       = 1 << 16
)

// defaultArticleCacheBytes is ArticleCacheSizeEnv when set and valid, otherwise
// sized from the memory this process may use.
func defaultArticleCacheBytes() int64 {
	if v, set := os.LookupEnv(ArticleCacheSizeEnv); set {
		if b, ok := parseArticleCacheMB(v); ok {
			return b
		}
		log.Printf("[usenet-stream] ignoring %s=%q: want a size in MiB between %d and %d", ArticleCacheSizeEnv, v, minArticleCacheMB, maxArticleCacheMB)
	}
	total, ok := sysinfo.TotalMemory()
	return articleCacheForMemory(total, ok)
}

// parseArticleCacheMB converts an ArticleCacheSizeEnv value to bytes.
func parseArticleCacheMB(v string) (int64, bool) {
	mb, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || mb < minArticleCacheMB || mb > maxArticleCacheMB {
		return 0, false
	}
	return mb << 20, true
}

// articleCacheForMemory sizes the cache from usable memory.
//
// The cache is what makes a re-read of a range served seconds ago free: a
// player's seek back, or ffmpeg reading the same bytes twice. That needs the
// last seconds of the stream plus its read-ahead, not minutes — 32 MiB is ~45
// articles of the usual ~716 KiB, 10-30 s of 1080p video, and even 16 MiB holds
// a player's current range and read-ahead several times over. More buys little
// and costs twice over, since the Go heap grows to about twice what stays live
// before a collection: the resident size of a stream is roughly 2 × cache plus
// ~40 MiB. An unknown size gets the floor.
func articleCacheForMemory(total uint64, known bool) int64 {
	if !known {
		return minDefaultArticleCache
	}
	return min(max(int64(total/articleCacheMemoryShare), minDefaultArticleCache), maxDefaultArticleCache)
}

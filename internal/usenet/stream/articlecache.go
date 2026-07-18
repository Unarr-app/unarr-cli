package stream

import "github.com/Unarr-app/unarr-cli/internal/usenet/yenc"

// articleCache is a bounded, LRU cache of decoded yEnc parts keyed by segment
// index. It is NOT internally synchronised: the owning Reader guards every call
// under its mutex so the cache and the in-flight set stay consistent with each
// other. Bounding matters because a decoded article is ~750 KB; an unbounded map
// during a long sequential stream would grow without limit.
type articleCache struct {
	capacity int
	parts    map[int]*yenc.Part
	order    []int // segment indices, oldest first (LRU eviction from the front)
}

// newArticleCache returns a cache holding at most capacity decoded articles
// (clamped to >= 1 so a misconfigured zero still caches the article in use).
func newArticleCache(capacity int) *articleCache {
	if capacity < 1 {
		capacity = 1
	}
	return &articleCache{
		capacity: capacity,
		parts:    make(map[int]*yenc.Part, capacity),
	}
}

// has reports whether segment seg is currently cached (without touching LRU).
func (c *articleCache) has(seg int) bool {
	_, ok := c.parts[seg]
	return ok
}

// get returns the cached part for seg and marks it most-recently-used.
func (c *articleCache) get(seg int) (*yenc.Part, bool) {
	p, ok := c.parts[seg]
	if ok {
		c.touch(seg)
	}
	return p, ok
}

// put stores part for seg as most-recently-used, evicting the least-recently-used
// entry when the cache is over capacity.
func (c *articleCache) put(seg int, p *yenc.Part) {
	if _, ok := c.parts[seg]; ok {
		c.parts[seg] = p
		c.touch(seg)
		return
	}
	c.parts[seg] = p
	c.order = append(c.order, seg)
	if len(c.order) > c.capacity {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.parts, oldest)
	}
}

// touch moves seg to the most-recently-used end of the order slice. O(n) in the
// (small, bounded) capacity — negligible next to a network fetch.
func (c *articleCache) touch(seg int) {
	for i, s := range c.order {
		if s == seg {
			c.order = append(c.order[:i], c.order[i+1:]...)
			c.order = append(c.order, seg)
			return
		}
	}
}

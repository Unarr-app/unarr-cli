package stream

import "testing"

func TestArticleCacheForMemory(t *testing.T) {
	const gib = 1 << 30
	cases := []struct {
		total uint64
		known bool
		want  int64
	}{
		{0, false, 16 << 20},
		{512 << 20, true, 16 << 20},
		{2 * gib, true, 16 << 20},
		{3 * gib, true, 24 << 20},
		{4 * gib, true, 32 << 20},
		{64 * gib, true, 32 << 20},
	}
	for _, c := range cases {
		if got := articleCacheForMemory(c.total, c.known); got != c.want {
			t.Errorf("articleCacheForMemory(%d, %v) = %d MiB, want %d MiB", c.total, c.known, got>>20, c.want>>20)
		}
	}
}

func TestReadaheadBoostForMemory(t *testing.T) {
	const gib = 1 << 30
	cases := []struct {
		total uint64
		known bool
		want  int64
	}{
		{0, false, 0},
		{1 * gib, true, 16 << 20},
		{4 * gib, true, 64 << 20},
		{64 * gib, true, 96 << 20},
	}
	for _, c := range cases {
		if got := readaheadBoostForMemory(c.total, c.known); got != c.want {
			t.Errorf("readaheadBoostForMemory(%d, %v) = %d MiB, want %d MiB", c.total, c.known, got>>20, c.want>>20)
		}
	}
}

func TestReadaheadBoostOffWithCacheOverride(t *testing.T) {
	t.Setenv(ArticleCacheSizeEnv, "48")
	if got := defaultReadaheadBoostBytes(); got != 0 {
		t.Fatalf("boost %d MiB with %s set, want none", got>>20, ArticleCacheSizeEnv)
	}
}

func TestParseArticleCacheMB(t *testing.T) {
	for v, want := range map[string]int64{"8": 8 << 20, "48": 48 << 20, " 128\n": 128 << 20} {
		if got, ok := parseArticleCacheMB(v); !ok || got != want {
			t.Errorf("parseArticleCacheMB(%q) = %d, %v; want %d", v, got, ok, want)
		}
	}
	for _, v := range []string{"", "-1", "0", "7", "abc", "32MB", "99999999"} {
		if _, ok := parseArticleCacheMB(v); ok {
			t.Errorf("parseArticleCacheMB(%q) accepted", v)
		}
	}
}

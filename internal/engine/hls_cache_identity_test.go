package engine

import "testing"

func TestHasCacheIdentity(t *testing.T) {
	cases := []struct {
		name string
		cfg  HLSSessionConfig
		want bool
	}{
		{"local file", HLSSessionConfig{SourcePath: "/m/a.mkv"}, true},
		{"url with cache id", HLSSessionConfig{SourceURL: "http://x/a", CacheID: "url:1"}, true},
		// The B1 case: keying on "" resolves to the daemon's cwd, the same key
		// for every IPTV title.
		{"url without cache id", HLSSessionConfig{SourceURL: "http://x/a"}, false},
		{"url with a stray path but no id", HLSSessionConfig{SourceURL: "http://x/a", SourcePath: "/m/a.mkv"}, false},
		{"nothing", HLSSessionConfig{}, false},
	}
	for _, c := range cases {
		if got := c.cfg.hasCacheIdentity(); got != c.want {
			t.Errorf("%s: hasCacheIdentity() = %v, want %v", c.name, got, c.want)
		}
	}
}

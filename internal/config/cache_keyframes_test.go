package config

import "testing"

// cache_keyframes gates the COPY-VOD keyframe index — the one sidecar playback
// cannot rebuild cheaply, and whose absence drops a session to EVENT copy, which
// ignores the resume position. Every config that predates the key must read as
// ENABLED; a zero-value false here would silently reproduce the original bug.
// (loadTOML lives in config_log_test.go.)

func TestCacheKeyframesDefaults(t *testing.T) {
	cases := []struct {
		name string
		toml string
		want bool
	}{
		{
			name: "key absent (pre-existing config) defaults on",
			toml: "[library]\nworkers = 4\n",
			want: true,
		},
		{
			name: "empty config defaults on",
			toml: "",
			want: true,
		},
		{
			name: "explicit true is honoured",
			toml: "[library]\ncache_keyframes = true\n",
			want: true,
		},
		{
			name: "explicit opt-out is honoured",
			toml: "[library]\ncache_keyframes = false\n",
			want: false,
		},
		{
			name: "opt-out survives other sidecars being on",
			toml: "[library]\ncache_keyframes = false\ncache_subtitles = true\ncache_thumbnails = true\n",
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := loadTOML(t, tc.toml).Library.CacheKeyframes; got != tc.want {
				t.Errorf("CacheKeyframes = %v, want %v", got, tc.want)
			}
		})
	}
}

// Turning keyframes off must not disturb the ffmpeg-driven sidecars, which are
// gated independently.
func TestCacheKeyframesIndependentOfOtherSidecars(t *testing.T) {
	cfg := loadTOML(t, "[library]\ncache_keyframes = false\n")
	if !cfg.Library.CacheSubtitles {
		t.Error("cache_subtitles should stay on its own default")
	}
	if !cfg.Library.CacheThumbnails {
		t.Error("cache_thumbnails should stay on its own default")
	}

	cfg = loadTOML(t, "[library]\ncache_subtitles = false\ncache_thumbnails = false\n")
	if !cfg.Library.CacheKeyframes {
		t.Error("disabling the ffmpeg sidecars must not disable the keyframe index")
	}
}

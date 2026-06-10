package engine

import "testing"

func TestDynamicReadahead(t *testing.T) {
	cases := []struct {
		name       string
		bitrateBps int64
		want       int64
	}{
		{"unknown bitrate → default", 0, defaultReadahead},
		{"negative → default", -1, defaultReadahead},
		{"low bitrate clamps to min", 1_000_000, minReadahead},                       // 1 Mbps → ~3.75 MiB < 8 MiB
		{"mid bitrate scales", 5_000_000, 5_000_000 / 8 * readaheadSeconds},          // 5 Mbps → ~18.75 MiB
		{"high bitrate within range", 25_000_000, 25_000_000 / 8 * readaheadSeconds}, // 4K ~25 Mbps → ~93.75 MiB
		{"very high clamps to max", 80_000_000, maxReadahead},                        // 80 Mbps → 300 MiB > cap
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := dynamicReadahead(c.bitrateBps)
			if got != c.want {
				t.Errorf("dynamicReadahead(%d) = %d, want %d", c.bitrateBps, got, c.want)
			}
			if got < minReadahead && c.bitrateBps > 0 {
				t.Errorf("result %d below min %d", got, minReadahead)
			}
			if got > maxReadahead {
				t.Errorf("result %d above max %d", got, maxReadahead)
			}
		})
	}
}

func TestDynamicReadahead_BeatsOldStatic(t *testing.T) {
	// The whole point: every result is bigger than the old static 5 MiB that
	// stalled HD/4K.
	const oldStatic = 5 * 1024 * 1024
	for _, b := range []int64{0, 1_000_000, 8_000_000, 25_000_000, 100_000_000} {
		if got := dynamicReadahead(b); got <= oldStatic {
			t.Errorf("dynamicReadahead(%d) = %d, not bigger than the old 5 MiB", b, got)
		}
	}
}

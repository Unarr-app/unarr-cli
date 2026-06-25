package library

import (
	"reflect"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/library/mediainfo"
)

func itemWithDuration(d float64) LibraryItem {
	return LibraryItem{
		FilePath:  "/m/x.mkv",
		MediaInfo: &mediainfo.MediaInfo{Video: &mediainfo.VideoInfo{Duration: d}},
	}
}

func TestThumbPositions(t *testing.T) {
	// Known duration → fractions (0.1/0.3/0.5/0.7/0.9) rounded to whole seconds.
	if got := thumbPositions(itemWithDuration(1000)); !reflect.DeepEqual(got, []float64{100, 300, 500, 700, 900}) {
		t.Errorf("dur=1000 → %v, want [100 300 500 700 900]", got)
	}

	// Unknown duration (no video info) → fixed fallback offsets.
	if got := thumbPositions(itemWithDuration(0)); !reflect.DeepEqual(got, []float64{30, 120, 300, 600, 1200}) {
		t.Errorf("dur=0 → %v, want fallback", got)
	}
	if got := thumbPositions(LibraryItem{FilePath: "/m/x.mkv"}); !reflect.DeepEqual(got, []float64{30, 120, 300, 600, 1200}) {
		t.Errorf("nil MediaInfo → %v, want fallback", got)
	}

	// Very short clip → multiple fractions round to the same second; deduped.
	// dur=2: round(0.2,0.6,1.0,1.4,1.8) = 0,1,1,1,2 → [0 1 2].
	if got := thumbPositions(itemWithDuration(2)); !reflect.DeepEqual(got, []float64{0, 1, 2}) {
		t.Errorf("dur=2 → %v, want [0 1 2] (deduped)", got)
	}
}

package mediainfo

import "testing"

// full-duration bytes for a given bitrate, used to build healthy vs short files.
func expectedBytes(bitRate int64, dur float64) int64 {
	return int64(float64(bitRate) / 8.0 * dur)
}

func TestTruncationVerdict(t *testing.T) {
	const dur = 1440.0 // 24-min anime episode
	const br = int64(5_000_000)
	full := expectedBytes(br, dur)

	// A decodeFail thunk that records whether it ran.
	makeConfirm := func(fails bool, ran *bool) func() bool {
		return func() bool {
			if ran != nil {
				*ran = true
			}
			return fails
		}
	}

	tests := []struct {
		name       string
		lastPTS    float64
		bitRate    int64
		fileSize   int64
		decodeFail func(ran *bool) func() bool
		wantReason string // "" = healthy (nil verdict)
		wantDecode bool   // whether check C should have run
	}{
		{
			name:       "healthy: tail reaches end, full size",
			lastPTS:    dur - 1, // last frame ~ at the end
			bitRate:    br,
			fileSize:   full,
			wantReason: "",
		},
		{
			name:       "truncated: tail stops at 7:36 (user case)",
			lastPTS:    456, // data ends far short of the 24-min header
			bitRate:    br,
			fileSize:   expectedBytes(br, 456),
			wantReason: "truncated",
		},
		{
			name:       "truncated: seek past EOF returned zero tail packets",
			lastPTS:    0,
			bitRate:    0, // MKV without header bit_rate
			fileSize:   full / 3,
			wantReason: "truncated",
		},
		{
			name:       "healthy: gap within threshold (credits trim ~20s)",
			lastPTS:    dur - 20,
			bitRate:    br,
			fileSize:   full,
			wantReason: "", // 20s < max(30, 3%·1440=43.2)
		},
		{
			name:       "tail_corrupt: full duration but byte-short and decode fails",
			lastPTS:    dur - 1,
			bitRate:    br,
			fileSize:   full / 2, // < 85% → shortfall
			decodeFail: func(ran *bool) func() bool { return makeConfirm(true, ran) },
			wantReason: "tail_corrupt",
			wantDecode: true,
		},
		{
			name:       "healthy: byte-short but decode succeeds (VBR, not damaged)",
			lastPTS:    dur - 1,
			bitRate:    br,
			fileSize:   full / 2,
			decodeFail: func(ran *bool) func() bool { return makeConfirm(false, ran) },
			wantReason: "",
			wantDecode: true,
		},
		{
			name:       "healthy: byte-short but no decoder available → don't guess",
			lastPTS:    dur - 1,
			bitRate:    br,
			fileSize:   full / 2,
			decodeFail: nil, // ffmpeg unavailable
			wantReason: "",
			wantDecode: false,
		},
		{
			name:       "no shortfall → decode confirm never runs",
			lastPTS:    dur - 1,
			bitRate:    br,
			fileSize:   full,
			decodeFail: func(ran *bool) func() bool { return makeConfirm(true, ran) },
			wantReason: "",
			wantDecode: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var ran bool
			var confirm func() bool
			if tc.decodeFail != nil {
				confirm = tc.decodeFail(&ran)
			}
			got := truncationVerdict(dur, tc.lastPTS, tc.bitRate, tc.fileSize, confirm)

			if tc.wantReason == "" {
				if got != nil {
					t.Fatalf("want healthy (nil), got %+v", got)
				}
			} else {
				if got == nil {
					t.Fatalf("want reason %q, got nil", tc.wantReason)
				}
				if !got.Damaged || got.Reason != tc.wantReason {
					t.Fatalf("want {Damaged:true Reason:%q}, got %+v", tc.wantReason, got)
				}
			}
			if ran != tc.wantDecode {
				t.Fatalf("decode-confirm ran=%v, want %v", ran, tc.wantDecode)
			}
		})
	}
}

func TestTailWindowSec(t *testing.T) {
	// Short film: frac·dur below the floor → floor wins.
	if got := tailWindowSec(600); got != truncTailFloorSec {
		t.Fatalf("tailWindowSec(600) = %v, want floor %v", got, truncTailFloorSec)
	}
	// Long film: 6% of 7200s = 432s dominates the 90s floor.
	if got := tailWindowSec(7200); got != 0.06*7200 {
		t.Fatalf("tailWindowSec(7200) = %v, want %v", got, 0.06*7200)
	}
	// The tail window must always exceed the gap threshold so a healthy tail read
	// spans past it (else a healthy file would look like it has a gap).
	for _, dur := range []float64{300, 1440, 3600, 10800} {
		win := tailWindowSec(dur)
		thr := maxFloat(truncGapFloorSec, truncGapFrac*dur)
		if win <= thr {
			t.Fatalf("dur=%v: tail window %v must exceed gap threshold %v", dur, win, thr)
		}
	}
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

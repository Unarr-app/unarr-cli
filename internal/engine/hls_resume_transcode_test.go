package engine

import "testing"

// A session refused COPY-VOD (HEVC/AV1 codec, Cast fMP4, remote without range,
// failed index) used to fall back to EVENT copy, which produces from t=0 and
// cannot be seeked: a resume at 59 min showed a black player until the linear
// remux got there. shouldTranscodeForResume decides when to spend CPU on a
// transcode — which honours -ss exactly — instead.

func TestShouldTranscodeForResume(t *testing.T) {
	cases := []struct {
		name     string
		startSec float64
		duration float64
		want     bool
	}{
		// The reported failure: Mickey 17 resumed at 3573 s of 8242 s.
		{"deep resume in a long film", 3573, 8242, true},
		{"resume just past the threshold", 121, 8242, true},

		// Near the start EVENT copy reaches the position almost immediately
		// (it remuxes at I/O speed), so a transcode would be the worse trade.
		{"no resume", 0, 8242, false},
		{"resume at the threshold is not past it", 120, 8242, false},
		{"trivial resume", 15, 8242, false},

		// A stale position at/past the end is handled by starting from 0; there is
		// nothing to seek to, so re-encoding for it is pure waste.
		{"resume exactly at duration", 8242, 8242, false},
		{"resume past duration (file replaced by a shorter cut)", 9000, 8242, false},

		// Without a known duration the seek target can't be validated.
		{"unknown duration", 3573, 0, false},
		{"negative duration", 3573, -1, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := HLSSessionConfig{StartSec: tc.startSec}
			probe := &StreamProbe{DurationSec: tc.duration}
			if got := shouldTranscodeForResume(cfg, probe); got != tc.want {
				t.Errorf("StartSec=%v duration=%v → %v, want %v",
					tc.startSec, tc.duration, got, tc.want)
			}
		})
	}
}

// A nil probe must not panic — StartHLSSession calls this on a path where the
// probe is expected but the guard has to hold regardless.
func TestShouldTranscodeForResumeNilProbe(t *testing.T) {
	if shouldTranscodeForResume(HLSSessionConfig{StartSec: 3573}, nil) {
		t.Error("nil probe → true, want false")
	}
}

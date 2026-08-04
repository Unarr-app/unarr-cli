package cmd

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/doctor"
	"github.com/Unarr-app/unarr-cli/internal/engine"
)

// runRow applies the doctor (message, error) convention so a row can be
// asserted on the status a user would actually see, not on its raw return.
func runRow(t *testing.T, fn func() (string, error)) doctor.Check {
	t.Helper()
	rep := doctor.Run([]doctor.Spec{{Group: "Media", Name: "row", Fn: fn}}, nil)
	return rep.Checks[0]
}

func TestMediaBinaryRow(t *testing.T) {
	tests := []struct {
		name       string
		tool       string
		found      string
		version    string
		configured string
		want       doctor.Status
		contains   string
	}{
		{
			name: "absent is a failure, not a warning",
			tool: "ffmpeg", want: doctor.StatusFail, contains: "install ffmpeg",
		},
		{
			// A typo in config.toml and "you never installed ffmpeg" have
			// completely different fixes, so they must not print the same line.
			name: "configured path that does not exist blames the config",
			tool: "ffmpeg", configured: "/opt/typo/ffmpeg",
			want: doctor.StatusFail, contains: "[library] ffmpeg_path",
		},
		{
			name: "present but not runnable is still a failure",
			tool: "ffprobe", found: "/usr/bin/ffprobe",
			want: doctor.StatusFail, contains: "did not run",
		},
		{
			name: "present reports path and version",
			tool: "ffmpeg", found: "/usr/bin/ffmpeg", version: "ffmpeg version 6.1.1",
			want: doctor.StatusPass, contains: "/usr/bin/ffmpeg (ffmpeg version 6.1.1)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := runRow(t, func() (string, error) {
				return mediaBinaryRow(tc.tool, tc.found, tc.version, tc.configured)
			})
			if c.Status != tc.want {
				t.Errorf("status = %q, want %q (message %q)", c.Status, tc.want, c.Message)
			}
			if !strings.Contains(c.Message, tc.contains) {
				t.Errorf("message %q does not contain %q", c.Message, tc.contains)
			}
		})
	}
}

func TestMediaEncodersRow(t *testing.T) {
	tests := []struct {
		name     string
		probe    engine.MediaProbe
		want     doctor.Status
		contains string
	}{
		{
			name:  "no ffmpeg cannot be verified",
			probe: engine.MediaProbe{},
			want:  doctor.StatusFail, contains: "no ffmpeg",
		},
		{
			name:  "encoder list unavailable",
			probe: engine.MediaProbe{FFmpegPath: "/usr/bin/ffmpeg"},
			want:  doctor.StatusFail, contains: "-encoders",
		},
		{
			name: "missing libx264 fails",
			probe: engine.MediaProbe{
				FFmpegPath: "/usr/bin/ffmpeg", EncodersProbed: true,
				MissingEncoders: []string{"libx264"},
			},
			want: doctor.StatusFail, contains: "missing libx264",
		},
		{
			name: "complete build passes",
			probe: engine.MediaProbe{
				FFmpegPath: "/usr/bin/ffmpeg", EncodersProbed: true,
			},
			want: doctor.StatusPass, contains: "libx264, aac",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := runRow(t, func() (string, error) { return mediaEncodersRow(tc.probe) })
			if c.Status != tc.want {
				t.Errorf("status = %q, want %q (message %q)", c.Status, tc.want, c.Message)
			}
			if !strings.Contains(c.Message, tc.contains) {
				t.Errorf("message %q does not contain %q", c.Message, tc.contains)
			}
		})
	}
}

// zscale and hwaccel are WARN-only: a host without either still plays video,
// just worse, so neither may turn the doctor run red.
func TestMediaWarnOnlyRows(t *testing.T) {
	ffmpeg := "/usr/bin/ffmpeg"
	tests := []struct {
		name     string
		fn       func() (string, error)
		want     doctor.Status
		contains string
	}{
		{
			name: "zscale absent warns",
			fn: func() (string, error) {
				return mediaZscaleRow(engine.MediaProbe{FFmpegPath: ffmpeg})
			},
			want: doctor.StatusWarn, contains: "not built in",
		},
		{
			name: "zscale present passes",
			fn: func() (string, error) {
				return mediaZscaleRow(engine.MediaProbe{FFmpegPath: ffmpeg, Zscale: true})
			},
			want: doctor.StatusPass, contains: "available",
		},
		{
			name: "no hwaccel at all warns",
			fn: func() (string, error) {
				return mediaHWAccelRow(engine.MediaProbe{
					FFmpegPath: ffmpeg,
					HW:         engine.HWAccelDiagnostic{Pick: engine.HWAccelNone},
				})
			},
			want: doctor.StatusWarn, contains: "no HW encoders compiled in",
		},
		{
			name: "encoders present but no device says so",
			fn: func() (string, error) {
				return mediaHWAccelRow(engine.MediaProbe{
					FFmpegPath: ffmpeg,
					HW: engine.HWAccelDiagnostic{
						Pick: engine.HWAccelNone, Encoders: []string{"h264_nvenc"},
					},
				})
			},
			want: doctor.StatusWarn, contains: "h264_nvenc compiled in but no matching device",
		},
		{
			name: "hwaccel picked passes with the codec it would use",
			fn: func() (string, error) {
				return mediaHWAccelRow(engine.MediaProbe{
					FFmpegPath: ffmpeg,
					HW: engine.HWAccelDiagnostic{
						Pick: engine.HWAccelNVENC, Devices: []string{"/dev/nvidia0"},
					},
				})
			},
			want: doctor.StatusPass, contains: "nvenc (h264_nvenc), devices /dev/nvidia0",
		},
		{
			name: "no ffmpeg leaves both unknown rather than failing",
			fn: func() (string, error) {
				return mediaHWAccelRow(engine.MediaProbe{})
			},
			want: doctor.StatusWarn, contains: "unknown",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := runRow(t, tc.fn)
			if c.Status != tc.want {
				t.Errorf("status = %q, want %q (message %q)", c.Status, tc.want, c.Message)
			}
			if !strings.Contains(c.Message, tc.contains) {
				t.Errorf("message %q does not contain %q", c.Message, tc.contains)
			}
		})
	}
}

// The ceiling row is informative: no state of the cache may fail the run, and
// no state may silently print nothing.
func TestMediaCeilingRowCacheStates(t *testing.T) {
	probe := engine.MediaProbe{
		FFmpegPath:    "/usr/bin/ffmpeg",
		FFmpegVersion: "ffmpeg version 6.1.1",
		HW:            engine.HWAccelDiagnostic{Pick: engine.HWAccelNone},
	}

	t.Run("cache absent", func(t *testing.T) {
		withDataDir(t)
		c := runRow(t, func() (string, error) { return mediaCeilingRow(probe) })
		assertInformative(t, c, "not measured")
		if !strings.Contains(c.Message, "unarr bench") {
			t.Errorf("an unmeasured ceiling must point at bench, got %q", c.Message)
		}
	})

	t.Run("cache fresh", func(t *testing.T) {
		withDataDir(t)
		key := engine.NewEncodeBenchKey(probe.FFmpegVersion, probe.HW.Pick)
		writeBench(t, key, 1080, "none")

		c := runRow(t, func() (string, error) { return mediaCeilingRow(probe) })
		assertInformative(t, c, "1080p via none")
	})

	t.Run("cache stale names what drifted", func(t *testing.T) {
		withDataDir(t)
		// Same host, older ffmpeg — exactly what an `apt upgrade` leaves behind.
		stale := engine.NewEncodeBenchKey("ffmpeg version 5.0.0", probe.HW.Pick)
		writeBench(t, stale, 720, "none")

		c := runRow(t, func() (string, error) { return mediaCeilingRow(probe) })
		assertInformative(t, c, "stale")
		for _, want := range []string{"720p", "ffmpeg", "unarr bench"} {
			if !strings.Contains(c.Message, want) {
				t.Errorf("stale message %q does not mention %q", c.Message, want)
			}
		}
	})

	t.Run("no ffmpeg", func(t *testing.T) {
		withDataDir(t)
		c := runRow(t, func() (string, error) { return mediaCeilingRow(engine.MediaProbe{}) })
		assertInformative(t, c, "install ffmpeg")
	})
}

func writeBench(t *testing.T, key engine.EncodeBenchKey, ceiling int, hw string) {
	t.Helper()
	res := engine.EncodeBenchmark{HWAccel: hw, Ceiling: ceiling, Reason: "test"}
	if err := engine.SaveEncodeBench(key, "test", res); err != nil {
		t.Fatalf("seed bench cache: %v", err)
	}
}

func assertInformative(t *testing.T, c doctor.Check, contains string) {
	t.Helper()
	if c.Status != doctor.StatusPass {
		t.Errorf("ceiling row must never fail or warn, got %q (%q)", c.Status, c.Message)
	}
	if !strings.Contains(c.Message, contains) {
		t.Errorf("message %q does not contain %q", c.Message, contains)
	}
}

// End to end over the real spec list: a host with no ffmpeg must NOT come back
// all-clear. That silent pass is the exact bug the Media block exists to close.
func TestMediaSpecsFailWithoutFFmpeg(t *testing.T) {
	withDataDir(t)
	// An explicit path that does not exist short-circuits the locator, so the
	// result is the same whether or not the developer's box has ffmpeg on PATH.
	missing := filepath.Join(t.TempDir(), "no-such-ffmpeg")
	cfg := config.Default()
	cfg.Library.FFmpegPath = missing
	cfg.Library.FFprobePath = missing

	specs := doctorMediaSpecs(&cfg)
	if len(specs) != 6 {
		t.Fatalf("got %d Media specs, want 6", len(specs))
	}

	start := time.Now()
	rep := doctor.Run(specs, nil)
	// Also a hang guard: no row may outlive the probe budget it declared.
	if elapsed := time.Since(start); elapsed > mediaProbeTimeout {
		t.Errorf("Media block took %s, longer than its own budget", elapsed)
	}
	if rep.Failed == 0 {
		t.Errorf("a host without ffmpeg reported no failures: %+v", rep.Checks)
	}
	for _, c := range rep.Checks {
		if c.Message == "" {
			t.Errorf("check %q rendered an empty message", c.Name)
		}
		if c.Status != doctor.StatusPass && c.Remedy == "" && c.Name != "Transcode ceiling" {
			t.Errorf("check %q is not passing but offers no remedy", c.Name)
		}
	}
}

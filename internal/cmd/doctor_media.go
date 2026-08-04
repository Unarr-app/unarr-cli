package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/doctor"
	"github.com/Unarr-app/unarr-cli/internal/engine"
	"github.com/Unarr-app/unarr-cli/internal/library/mediainfo"
)

// The Media block exists because a host without ffmpeg used to pass doctor with
// "All checks passed!" and then play nothing: HLS, thumbnails, trickplay,
// `library stats --quality` and subtitle burn-in all shell out to it. Config
// being perfect is worth very little if the transcode chain is absent.
const mediaGroup = "Media"

// mediaProbeTimeout bounds the ENTIRE ffmpeg interrogation, not one call. doctor
// is interactive; a wedged binary (a half-written download, an NFS mount that
// went away) must cost seconds, not the session.
const mediaProbeTimeout = 15 * time.Second

// ffmpegInstallHint is the remedy for a missing binary. Deliberately NOT
// remedyDoctorFix: nothing in planRepairs installs or fetches ffmpeg, and
// pointing at `doctor --fix` would send the user to a command that then reports
// nothing to repair. Auto-download is out of scope by design — doctor detects
// and guides.
const ffmpegInstallHint = "install ffmpeg (apt install ffmpeg / brew install ffmpeg), " +
	"use the unarr release tarball (ffmpeg ships next to the binary), " +
	"or set the path in config.toml under [library]"

// doctorMediaSpecs builds the Media rows. All six read ONE lazily-taken probe:
// sync.OnceValue means the ffmpeg subprocesses are paid once, and only if the
// block is actually reached, while each row still renders independently as the
// text renderer streams them.
func doctorMediaSpecs(cfg *config.Config) []doctor.Spec {
	probe := sync.OnceValue(func() engine.MediaProbe {
		ctx, cancel := context.WithTimeout(context.Background(), mediaProbeTimeout)
		defer cancel()
		// LocateFFmpeg, not ResolveFFmpeg: the latter falls back to downloading
		// ~50 MB, which would turn a diagnostic into a silent installer and
		// report "present" for a host that had nothing a moment earlier.
		ffmpeg, _ := mediainfo.LocateFFmpeg(cfg.Library.FFmpegPath)
		ffprobe, _ := mediainfo.LocateFFprobe(cfg.Library.FFprobePath)
		return engine.ProbeMedia(ctx, ffmpeg, ffprobe)
	})

	return []doctor.Spec{
		{
			Group:  mediaGroup,
			Name:   "ffmpeg",
			Remedy: ffmpegInstallHint,
			Fn: func() (string, error) {
				p := probe()
				return mediaBinaryRow("ffmpeg", p.FFmpegPath, p.FFmpegVersion, cfg.Library.FFmpegPath)
			},
		},
		{
			Group:  mediaGroup,
			Name:   "ffprobe",
			Remedy: ffmpegInstallHint,
			Fn: func() (string, error) {
				p := probe()
				return mediaBinaryRow("ffprobe", p.FFprobePath, p.FFprobeVersion, cfg.Library.FFprobePath)
			},
		},
		{
			Group:  mediaGroup,
			Name:   "Encoders (libx264, aac)",
			Remedy: "install an ffmpeg built with libx264 and aac — both the distro package and the bundled build have them",
			Fn:     func() (string, error) { return mediaEncodersRow(probe()) },
		},
		{
			Group:  mediaGroup,
			Name:   "zscale (HDR tonemap)",
			Remedy: "install an ffmpeg built with libzimg (--enable-libzimg); without it HDR sources are not tonemapped",
			Fn:     func() (string, error) { return mediaZscaleRow(probe()) },
		},
		{
			Group:  mediaGroup,
			Name:   "Hardware acceleration",
			Remedy: "run `unarr probe-hwaccel` for the full picture — a software-only ffmpeg build is the usual cause, not a missing GPU",
			Fn:     func() (string, error) { return mediaHWAccelRow(probe()) },
		},
		{
			Group: mediaGroup,
			Name:  "Transcode ceiling",
			Fn:    func() (string, error) { return mediaCeilingRow(probe()) },
		},
	}
}

// mediaBinaryRow renders the presence row for one binary. `configured` is the
// [library] path the operator set, so a typo there is reported as a typo rather
// than as "not installed" — the two have completely different fixes.
func mediaBinaryRow(tool, found, version, configured string) (string, error) {
	if found == "" {
		if configured != "" {
			return fmt.Sprintf("[library] %s_path = %q does not exist", tool, configured),
				fmt.Errorf("%s not found at the configured path", tool)
		}
		return "not found — " + ffmpegInstallHint, fmt.Errorf("%s not found", tool)
	}
	if version == "" {
		// Present but not runnable: wrong architecture, a missing shared
		// library, or a binary wedged long enough to blow the probe budget. It
		// would fail the same way mid-playback, so this is not a pass.
		return fmt.Sprintf("%s — found, but `%s -version` did not run", found, tool),
			fmt.Errorf("%s is not executable", tool)
	}
	return fmt.Sprintf("%s (%s)", found, version), nil
}

func mediaEncodersRow(p engine.MediaProbe) (string, error) {
	switch {
	case p.FFmpegPath == "":
		return "cannot verify — no ffmpeg", errors.New("no ffmpeg to query")
	case !p.EncodersProbed:
		return "`ffmpeg -encoders` returned nothing — this build is not usable", errors.New("encoder list unavailable")
	case len(p.MissingEncoders) > 0:
		return "missing " + strings.Join(p.MissingEncoders, ", ") +
			" — this ffmpeg cannot produce browser-playable HLS", errors.New("required encoder missing")
	}
	return strings.Join(engine.RequiredEncoders, ", "), nil
}

func mediaZscaleRow(p engine.MediaProbe) (string, error) {
	if p.FFmpegPath == "" {
		return "!unknown — no ffmpeg to query", nil
	}
	if !p.Zscale {
		return "!not built in — HDR sources play without tonemapping (washed out)", nil
	}
	return "available — HDR→SDR tonemapping enabled", nil
}

func mediaHWAccelRow(p engine.MediaProbe) (string, error) {
	if p.FFmpegPath == "" {
		return "!unknown — no ffmpeg to query", nil
	}
	if p.HW.Pick == engine.HWAccelNone {
		// Encoders-but-no-device is a different story than no-encoders: the
		// first is usually a container missing a device mapping, the second an
		// ffmpeg build. Saying which one saves the round trip.
		reason := "no HW encoders compiled in"
		if len(p.HW.Encoders) > 0 {
			reason = strings.Join(p.HW.Encoders, ", ") + " compiled in but no matching device"
		}
		return "!none — software libx264 only (" + reason + "); see `unarr probe-hwaccel`", nil
	}
	summary := fmt.Sprintf("%s (%s)", p.HW.Pick, p.HW.Pick.FFmpegVideoCodec("h264"))
	if len(p.HW.Devices) > 0 {
		summary += ", devices " + strings.Join(p.HW.Devices, ", ")
	}
	return summary, nil
}

// mediaCeilingRow REPORTS the cached `unarr bench --encode` measurement and
// never takes one: measuring costs ~20 s of real encoding, which doctor must
// not spend. Absent or stale therefore has to be said out loud and pointed at
// the command that fixes it — printing nothing would read as "fine".
//
// Informative only: every branch returns a nil error and no '!' prefix. Never
// having run a benchmark is not a fault of the host.
func mediaCeilingRow(p engine.MediaProbe) (string, error) {
	if p.FFmpegPath == "" {
		return "not measured — install ffmpeg, then run `unarr bench --encode`", nil
	}
	key := engine.NewEncodeBenchKey(p.FFmpegVersion, p.HW.Pick)
	rec, fresh := engine.LoadEncodeBench(key)
	if rec.MeasuredAt.IsZero() {
		return "not measured — run `unarr bench --encode` to record it", nil
	}
	age := formatDuration(time.Since(rec.MeasuredAt))
	if fresh {
		return fmt.Sprintf("%dp via %s (measured %s ago)", rec.Result.Ceiling, rec.Result.HWAccel, age), nil
	}
	drift := key.DriftedFrom(rec.Key)
	if len(drift) == 0 {
		drift = []string{"the host fingerprint"}
	}
	return fmt.Sprintf("stale — %dp measured %s ago, %s changed since; re-run `unarr bench --encode`",
		rec.Result.Ceiling, age, strings.Join(drift, ", ")), nil
}

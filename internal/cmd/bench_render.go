package cmd

import (
	"fmt"

	"github.com/Unarr-app/unarr-cli/internal/engine"
	"github.com/fatih/color"
)

// benchLabelWidth aligns the section labels into the same column as the
// continuation lines, so the eye reads one block per subsystem.
const benchLabelWidth = 12

func renderBench(rep benchReport) {
	bold := color.New(color.Bold)
	dim := color.New(color.FgHiBlack)
	yellow := color.New(color.FgYellow)

	fmt.Println()
	bold.Println("  unarr bench")
	fmt.Println()

	if rep.Encode != nil {
		renderEncodeSection(*rep.Encode)
	}
	if rep.Disk != nil {
		renderDiskSection(*rep.Disk)
	}
	if rep.Net != nil {
		renderNetSection(*rep.Net)
	}
	for _, note := range rep.Notes {
		yellow.Printf("  !  %s\n", note)
	}
	if len(rep.Notes) > 0 {
		fmt.Println()
	}
	if rep.Encode != nil {
		dim.Printf("  cached for 'unarr doctor' at %s\n\n", shortenHome(rep.Encode.CachePath))
	}
}

func renderEncodeSection(s encodeSection) {
	bold := color.New(color.Bold)
	dim := color.New(color.FgHiBlack)

	bold.Printf("  %-*s", benchLabelWidth, "Encode")
	fmt.Printf("%-18s -> %dp\n", encodeBackendLabel(s.Result), s.Result.Ceiling)

	for _, r := range s.Result.Rungs {
		fmt.Printf("  %-*s", benchLabelWidth, "")
		if !r.Measured {
			dim.Printf("%dp  probe failed (ffmpeg/lavfi)\n", r.Height)
			continue
		}
		verdict := "below threshold"
		if r.Factor >= s.Result.Threshold {
			verdict = "sustainable"
		}
		dim.Printf("%dp  %.1fx realtime  (threshold %.1fx) — %s\n",
			r.Height, r.Factor, s.Result.Threshold, verdict)
	}

	fmt.Printf("  %-*s", benchLabelWidth, "")
	dim.Printf("verdict: %s\n", encodeVerdict(s.Result))
	if s.FFmpegVersion != "" {
		fmt.Printf("  %-*s", benchLabelWidth, "")
		dim.Printf("%s\n", s.FFmpegVersion)
	}
	fmt.Println()
}

// encodeBackendLabel names what did the encoding, because "720p" alone never
// tells the user whether the fix is a better CPU or a better ffmpeg build.
func encodeBackendLabel(res engine.EncodeBenchmark) string {
	if res.HWAccel == string(engine.HWAccelNone) || res.HWAccel == "" {
		return "software libx264"
	}
	return "hwaccel " + res.HWAccel
}

// encodeVerdict turns the ceiling into the sentence the user actually needs:
// what happens to a source LARGER than the ceiling.
func encodeVerdict(res engine.EncodeBenchmark) string {
	switch res.Reason {
	case engine.EncodeReasonHardware:
		return fmt.Sprintf("4K via %s OK; without that hwaccel this host falls back to software", res.HWAccel)
	case engine.EncodeReasonUnmeasurable:
		return "the probe could not run — this is the historical 1080p default, not a measurement"
	case engine.EncodeReasonFloor:
		return "this host cannot sustain even 480p software encode; anything larger goes to an external player"
	case engine.EncodeReasonNoFFmpeg:
		return "no ffmpeg — nothing was measured"
	default:
		return fmt.Sprintf("%dp and below transcode in real time; larger sources go to an external player", res.Ceiling)
	}
}

func renderDiskSection(d engine.DiskBenchResult) {
	bold := color.New(color.Bold)
	dim := color.New(color.FgHiBlack)

	bold.Printf("  %-*s", benchLabelWidth, "Disk")
	fmt.Printf("%s  %.0f MB/s sequential write\n", shortenHome(d.Dir), d.MBPerSec)
	fmt.Printf("  %-*s", benchLabelWidth, "")
	dim.Printf("%d MiB written and fsynced in %.1fs (page cache excluded)\n",
		d.Bytes/(1<<20), d.Seconds)
	fmt.Println()
}

func renderNetSection(n engine.NetBenchResult) {
	bold := color.New(color.Bold)
	dim := color.New(color.FgHiBlack)

	bold.Printf("  %-*s", benchLabelWidth, "Net")
	fmt.Printf("daemon %s  %.0f Mbps · /health %.1f ms\n", n.BaseURL, n.Mbps, n.LatencyMS)
	fmt.Printf("  %-*s", benchLabelWidth, "")
	dim.Printf("loopback — how fast the agent serves bytes, not your internet link\n")
	fmt.Println()
}

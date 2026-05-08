package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/torrentclaw/unarr/internal/engine"
)

// newProbeHWAccelCmd reports the hardware-acceleration capabilities the daemon
// would actually use for HLS/WebRTC transcoding. The motivation: a beefy host
// (e.g. RTX 3090) can still fall back to software encoding when the installed
// ffmpeg binary was built without nvenc/qsv/vaapi support — Homebrew ffmpeg
// is a common offender. Without this command, users see slow / failing 4K
// transcodes and no obvious way to diagnose where the regression sits.
func newProbeHWAccelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "probe-hwaccel",
		Short: "Diagnose hardware-acceleration availability",
		Long: `Report the hardware-acceleration backends the daemon would pick for
transcoding, plus exactly why each one was kept or rejected.

Checks performed:
  - ffmpeg / ffprobe paths
  - which HW encoders the ffmpeg binary supports (h264_nvenc, h264_qsv, h264_vaapi…)
  - whether the matching device files / drivers are actually present
  - which backend the daemon would pick today (HWAccelNone means software)

Use this when transcoding feels slow or fails on 4K — the most common cause
is a software-only ffmpeg build, not a missing GPU.`,
		Example: `  unarr probe-hwaccel`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProbeHWAccel()
		},
	}
}

func runProbeHWAccel() error {
	bold := color.New(color.Bold)
	green := color.New(color.FgGreen)
	yellow := color.New(color.FgYellow)
	red := color.New(color.FgRed)

	fmt.Println()
	bold.Println("  Hardware acceleration probe")
	fmt.Println()

	// 1. Locate ffmpeg / ffprobe.
	ffmpegPath, ffmpegErr := exec.LookPath("ffmpeg")
	ffprobePath, ffprobeErr := exec.LookPath("ffprobe")

	bold.Println("  Binaries")
	if ffmpegErr != nil {
		red.Printf("    x ffmpeg not on PATH\n")
		fmt.Println()
		yellow.Println("    HW probe needs ffmpeg. Install it:")
		fmt.Println("      Ubuntu/Debian: sudo apt install ffmpeg")
		fmt.Println("      macOS:         brew install ffmpeg")
		fmt.Println()
		return nil
	}
	green.Printf("    OK ffmpeg  %s\n", ffmpegPath)
	if ffprobeErr != nil {
		yellow.Printf("    !  ffprobe not on PATH (HLS still works, source probing falls back to ffmpeg)\n")
	} else {
		green.Printf("    OK ffprobe %s\n", ffprobePath)
	}
	fmt.Println()

	// 2. List encoders the ffmpeg binary supports.
	bold.Println("  HW encoders compiled in")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-encoders").CombinedOutput()
	if err != nil {
		red.Printf("    x ffmpeg -encoders failed: %v\n", err)
		fmt.Println()
		return nil
	}
	encoders := string(out)

	hwEncoders := []struct {
		name    string
		family  string
		family2 string
	}{
		{"h264_nvenc", "NVIDIA NVENC", "hevc_nvenc"},
		{"h264_qsv", "Intel Quick Sync", "hevc_qsv"},
		{"h264_vaapi", "Linux VA-API (Intel/AMD)", "hevc_vaapi"},
		{"h264_videotoolbox", "macOS VideoToolbox", "hevc_videotoolbox"},
	}
	anyHWEncoder := false
	for _, e := range hwEncoders {
		hasH264 := strings.Contains(encoders, e.name)
		hasHEVC := strings.Contains(encoders, e.family2)
		if hasH264 || hasHEVC {
			anyHWEncoder = true
			green.Printf("    OK %s\n", e.family)
			if hasH264 {
				fmt.Printf("       %s\n", e.name)
			}
			if hasHEVC {
				fmt.Printf("       %s\n", e.family2)
			}
		}
	}
	if !anyHWEncoder {
		red.Printf("    x No HW encoders compiled in\n")
		fmt.Println()
		yellow.Println("    Most likely your ffmpeg was built without --enable-nvenc /")
		yellow.Println("    --enable-libmfx / --enable-vaapi. Brew's default formula is one")
		yellow.Println("    common offender. On Ubuntu, the system package ships with VAAPI")
		yellow.Println("    by default and NVENC if you have CUDA installed.")
	}
	fmt.Println()

	// 3. Device-file checks.
	bold.Println("  Devices / drivers")
	checks := []struct {
		path string
		desc string
	}{
		{"/dev/nvidia0", "NVIDIA GPU"},
		{"/dev/dri/renderD128", "Linux DRM render node (used by VA-API + QSV)"},
	}
	for _, c := range checks {
		if fileExistsLocal(c.path) {
			green.Printf("    OK %s — %s\n", c.path, c.desc)
		} else {
			yellow.Printf("    -  %s — %s (not present)\n", c.path, c.desc)
		}
	}
	if _, err := exec.LookPath("nvidia-smi"); err == nil {
		green.Printf("    OK nvidia-smi on PATH\n")
	} else {
		yellow.Printf("    -  nvidia-smi not on PATH\n")
	}
	if runtime.GOOS == "darwin" {
		fmt.Printf("    .  macOS host — VideoToolbox available if encoder was compiled in\n")
	}
	fmt.Println()

	// 4. Daemon's actual decision.
	engine.ResetHWAccelCache()
	pick := engine.DetectHWAccel(ctx, ffmpegPath)
	bold.Println("  Daemon would pick")
	switch pick {
	case engine.HWAccelNone:
		red.Printf("    x %s — software libx264 only\n", pick)
		fmt.Println()
		yellow.Println("    On a slow CPU 1080p will lag and 4K is effectively unwatchable.")
		yellow.Println("    Fix: rebuild / reinstall ffmpeg with HW encoder support, then:")
		fmt.Println()
		fmt.Println("      unarr daemon restart")
	default:
		green.Printf("    OK %s\n", pick)
		fmt.Printf("    encoder: %s (h264) / %s (hevc)\n", pick.FFmpegVideoCodec("h264"), pick.FFmpegVideoCodec("hevc"))
	}
	fmt.Println()

	return nil
}

// fileExistsLocal stats a path. Mirrors engine.fileExists without exporting it.
func fileExistsLocal(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

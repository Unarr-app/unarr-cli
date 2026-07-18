package mediainfo

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// ResolveFFmpeg finds the ffmpeg binary. Search order mirrors ResolveFFprobe
// so the same operator setup works for both:
//  1. Explicit path (--ffmpeg flag / library.ffmpeg_path config)
//  2. FFMPEG_PATH env var
//  3. "ffmpeg" on PATH
//  4. Adjacent to the current executable (release tarball bundles ffmpeg
//     next to the unarr binary — this is the preferred install path)
//  5. Previously downloaded in the unarr cache dir
//  6. Auto-download static binary as last resort (~50MB, slow start)
//
// ffmpeg is required for the HLS streaming pipeline; ffprobe alone can't
// transcode HEVC/MKV to browser-friendly H.264/MP4 fragments.
func ResolveFFmpeg(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return explicit, nil
		}
		return "", fmt.Errorf("ffmpeg not found at explicit path: %s", explicit)
	}

	if envPath := os.Getenv("FFMPEG_PATH"); envPath != "" {
		if _, err := os.Stat(envPath); err == nil {
			return envPath, nil
		}
	}

	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p, nil
	}

	if exePath, err := os.Executable(); err == nil {
		name := "ffmpeg"
		if runtime.GOOS == "windows" {
			name = "ffmpeg.exe"
		}
		adjacent := filepath.Join(filepath.Dir(exePath), name)
		if _, err := os.Stat(adjacent); err == nil {
			return adjacent, nil
		}
	}

	if cached, err := FFmpegCachePath(); err == nil {
		if _, err := os.Stat(cached); err == nil {
			return cached, nil
		}
	}

	if p, err := DownloadFFmpeg(); err == nil {
		return p, nil
	}

	if isDocker() {
		return "", fmt.Errorf(
			"ffmpeg not found and auto-download failed (read-only filesystem?).\n" +
				"Options:\n" +
				"  • Use the official image: unarr/cli (includes ffmpeg)\n" +
				"  • Set FFMPEG_PATH env var to point to a pre-installed ffmpeg binary\n" +
				"  • Add to config.toml: [library]\\nffmpeg_path = \"/path/to/ffmpeg\"",
		)
	}
	return "", fmt.Errorf(
		"ffmpeg not found and auto-download failed.\n" +
			"Options:\n" +
			"  • Install ffmpeg: sudo apt install ffmpeg  (or brew install ffmpeg)\n" +
			"  • Use the unarr release tarball — ffmpeg is bundled next to the binary\n" +
			"  • Set FFMPEG_PATH env var to point to the ffmpeg binary\n" +
			"  • Add to config.toml: [library]\\nffmpeg_path = \"/path/to/ffmpeg\"",
	)
}

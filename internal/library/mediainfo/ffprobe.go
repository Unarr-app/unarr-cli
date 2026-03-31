package mediainfo

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// ffprobeOutput matches the JSON structure from `ffprobe -show_streams -show_format`.
type ffprobeOutput struct {
	Streams []ffprobeStream `json:"streams"`
	Format  ffprobeFormat   `json:"format"`
}

type ffprobeFormat struct {
	Duration string `json:"duration"`
}

type ffprobeStream struct {
	CodecType      string            `json:"codec_type"`
	CodecName      string            `json:"codec_name"`
	Profile        string            `json:"profile"`
	Channels       int               `json:"channels"`
	Width          int               `json:"width"`
	Height         int               `json:"height"`
	BitsPerRaw     string            `json:"bits_per_raw_sample"`
	PixFmt         string            `json:"pix_fmt"`
	ColorSpace     string            `json:"color_space"`
	ColorTransfer  string            `json:"color_transfer"`
	ColorPrimaries string            `json:"color_primaries"`
	RFrameRate     string            `json:"r_frame_rate"`
	Duration       string            `json:"duration"`
	Tags           map[string]string `json:"tags"`
	Disposition    map[string]int    `json:"disposition"`
	SideDataList   []sideData        `json:"side_data_list"`
}

type sideData struct {
	SideDataType string `json:"side_data_type"`
}

// hdrProfiles maps (color_space, color_transfer) to HDR type.
var hdrProfiles = map[[2]string]string{
	{"bt2020nc", "smpte2084"}:    "HDR10",
	{"bt2020nc", "arib-std-b67"}: "HLG",
}

// ExtractMediaInfo runs ffprobe on a file and parses audio, subtitle, and video streams.
func ExtractMediaInfo(ctx context.Context, ffprobePath, filePath string) (*MediaInfo, error) {
	cmd := exec.CommandContext(ctx, ffprobePath,
		"-v", "error",
		"-print_format", "json",
		"-show_streams",
		"-show_format",
		filePath,
	)

	var stderr strings.Builder
	cmd.Stderr = &stderr

	output, err := cmd.Output()
	if err != nil {
		if _, statErr := os.Stat(filePath); statErr != nil {
			return nil, fmt.Errorf("ffprobe: file not found: %s", filePath)
		}
		return nil, fmt.Errorf("ffprobe failed (file=%s): %s", filePath, stderr.String())
	}

	var data ffprobeOutput
	if err := json.Unmarshal(output, &data); err != nil {
		return nil, fmt.Errorf("ffprobe JSON parse failed: %w", err)
	}

	return parseFFprobeOutput(data)
}

// parseFFprobeOutput converts parsed ffprobe JSON into MediaInfo.
// Separated from ExtractMediaInfo so it can be tested without running ffprobe.
func parseFFprobeOutput(data ffprobeOutput) (*MediaInfo, error) {
	if len(data.Streams) == 0 {
		return nil, fmt.Errorf("ffprobe returned no streams")
	}

	var audioTracks []AudioTrack
	var subtitleTracks []SubtitleTrack
	var videoInfo *VideoInfo

	for _, s := range data.Streams {
		switch s.CodecType {
		case "audio":
			langRaw := tagValue(s.Tags, "language")
			track := AudioTrack{
				Lang:     NormalizeLang(langRaw),
				Codec:    s.CodecName,
				Channels: s.Channels,
			}
			if title := tagValue(s.Tags, "title"); title != "" {
				track.Title = title
			}
			if s.Disposition["default"] == 1 {
				track.Default = true
			}
			audioTracks = append(audioTracks, track)

		case "subtitle":
			langRaw := tagValue(s.Tags, "language")
			track := SubtitleTrack{
				Lang:  NormalizeLang(langRaw),
				Codec: s.CodecName,
			}
			if title := tagValue(s.Tags, "title"); title != "" {
				track.Title = title
			}
			if s.Disposition["forced"] == 1 {
				track.Forced = true
			}
			subtitleTracks = append(subtitleTracks, track)

		case "video":
			if videoInfo != nil {
				continue // only first video stream
			}
			vi := &VideoInfo{
				Codec:  s.CodecName,
				Width:  s.Width,
				Height: s.Height,
			}

			// Bit depth
			if s.BitsPerRaw != "" {
				if bd, err := strconv.Atoi(s.BitsPerRaw); err == nil {
					vi.BitDepth = bd
				}
			} else if containsAny(s.PixFmt, "10le", "10be", "p010") {
				vi.BitDepth = 10
			} else if containsAny(s.PixFmt, "12le", "12be") {
				vi.BitDepth = 12
			}

			// HDR detection
			hdrKey := [2]string{s.ColorSpace, s.ColorTransfer}
			if hdr, ok := hdrProfiles[hdrKey]; ok {
				vi.HDR = hdr
			} else if s.ColorTransfer == "smpte2084" {
				vi.HDR = "HDR10"
			} else if s.ColorTransfer == "arib-std-b67" {
				vi.HDR = "HLG"
			}

			// Dolby Vision via side_data_list
			for _, sd := range s.SideDataList {
				if sd.SideDataType == "DOVI configuration record" {
					if vi.HDR != "" {
						vi.HDR = "DV+" + vi.HDR
					} else {
						vi.HDR = "DV"
					}
					break
				}
			}

			// Frame rate from r_frame_rate (e.g., "24000/1001")
			if s.RFrameRate != "" && strings.Contains(s.RFrameRate, "/") {
				parts := strings.SplitN(s.RFrameRate, "/", 2)
				if num, err1 := strconv.ParseFloat(parts[0], 64); err1 == nil {
					if den, err2 := strconv.ParseFloat(parts[1], 64); err2 == nil && den > 0 {
						vi.FrameRate = math.Round(num/den*1000) / 1000
					}
				}
			}

			// Profile
			if s.Profile != "" {
				vi.Profile = s.Profile
			}

			// Duration: prefer format.duration, fallback to stream duration
			if dur := parseDuration(data.Format.Duration); dur > 0 {
				vi.Duration = dur
			} else if dur := parseDuration(s.Duration); dur > 0 {
				vi.Duration = dur
			}

			videoInfo = vi
		}
	}

	result := &MediaInfo{
		Video: videoInfo,
	}
	if len(audioTracks) > 0 {
		result.Audio = audioTracks
		result.Languages = ComputeLanguages(audioTracks)
	}
	if len(subtitleTracks) > 0 {
		result.Subtitles = subtitleTracks
	}
	return result, nil
}

// ResolveFFprobe finds the ffprobe binary. Search order:
// 1. Explicit path (--ffprobe flag)
// 2. FFPROBE_PATH env var
// 3. "ffprobe" in PATH
// 4. Adjacent to the current executable
// 5. Previously downloaded in cache dir
// 6. Auto-download static binary
func ResolveFFprobe(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return explicit, nil
		}
		return "", fmt.Errorf("ffprobe not found at explicit path: %s", explicit)
	}

	if envPath := os.Getenv("FFPROBE_PATH"); envPath != "" {
		if _, err := os.Stat(envPath); err == nil {
			return envPath, nil
		}
	}

	if p, err := exec.LookPath("ffprobe"); err == nil {
		return p, nil
	}

	if exePath, err := os.Executable(); err == nil {
		name := "ffprobe"
		if runtime.GOOS == "windows" {
			name = "ffprobe.exe"
		}
		adjacent := filepath.Join(filepath.Dir(exePath), name)
		if _, err := os.Stat(adjacent); err == nil {
			return adjacent, nil
		}
	}

	if cached, err := FFprobeCachePath(); err == nil {
		if _, err := os.Stat(cached); err == nil {
			return cached, nil
		}
	}

	if p, err := DownloadFFprobe(); err == nil {
		return p, nil
	}

	return "", fmt.Errorf("ffprobe not found. Install ffmpeg or provide --ffprobe path")
}

// tagValue gets a tag value case-insensitively.
func tagValue(tags map[string]string, key string) string {
	if v, ok := tags[key]; ok {
		return v
	}
	if v, ok := tags[strings.ToUpper(key)]; ok {
		return v
	}
	return ""
}

func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// parseDuration converts a duration string (e.g. "7423.500000") to float64 seconds.
func parseDuration(s string) float64 {
	if s == "" {
		return 0
	}
	d, err := strconv.ParseFloat(s, 64)
	if err != nil || d <= 0 {
		return 0
	}
	return math.Round(d*1000) / 1000
}

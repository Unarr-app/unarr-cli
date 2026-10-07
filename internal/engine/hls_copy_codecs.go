package engine

import (
	"fmt"
	"slices"
	"strings"
)

// normalizeVideoCodec maps a codec name onto ffprobe's codec_name vocabulary
// ("h264", "hevc", "av1", …) so the browser list the web sends and the probed
// source compare equal whatever spelling either side used.
func normalizeVideoCodec(codec string) string {
	c := strings.ToLower(strings.TrimSpace(codec))
	switch c {
	case "h265", "hvc1", "hev1", "x265":
		return "hevc"
	case "avc", "avc1", "x264":
		return "h264"
	}
	return c
}

// copyVideoCodecAllowed reports whether an HLS-copy session may copy the probed
// source video for a browser that decodes browserCodecs natively. An empty list
// is no constraint (a web that predates the field, or one that skipped it): copy
// as before. h264 additionally needs bit depth <= 8 — High 10 plays almost
// nowhere in a browser, even where 8-bit h264 does.
func copyVideoCodecAllowed(browserCodecs []string, sourceCodec string, bitDepth int) bool {
	if len(browserCodecs) == 0 {
		return true
	}
	src := normalizeVideoCodec(sourceCodec)
	if src == "" {
		return false // unknown source codec: copying blind is how black screens happen
	}
	if src == "h264" && bitDepth > 8 {
		return false
	}
	return slices.ContainsFunc(browserCodecs, func(c string) bool { return normalizeVideoCodec(c) == src })
}

// copyVideoCodecLabel names the source for the "not in browser codecs" log,
// including the bit depth when that (not the codec) is why copy was refused.
func copyVideoCodecLabel(sourceCodec string, bitDepth int) string {
	src := normalizeVideoCodec(sourceCodec)
	if src == "" {
		return "unknown"
	}
	if src == "h264" && bitDepth > 8 {
		return fmt.Sprintf("%s %d-bit", src, bitDepth)
	}
	return src
}

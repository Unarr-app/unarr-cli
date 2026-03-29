package library

import (
	"regexp"
	"strings"

	"github.com/torrentclaw/torrentclaw-cli/internal/library/mediainfo"
)

var (
	seasonRegex  = regexp.MustCompile(`(?i)S(\d{1,2})E(\d{1,2})`)
	seasonOnly   = regexp.MustCompile(`(?i)S(\d{1,2})(?:\b|$)`)
	altEpRegex   = regexp.MustCompile(`(?i)(\d{1,2})x(\d{2})`)
)

// ResolveResolution maps a pixel height to a standard resolution label.
func ResolveResolution(height int) string {
	switch {
	case height >= 2000:
		return "2160p"
	case height >= 900:
		return "1080p"
	case height >= 600:
		return "720p"
	case height >= 400:
		return "480p"
	default:
		return ""
	}
}

// DeriveContentType guesses "movie" or "show" from parsed metadata.
func DeriveContentType(item LibraryItem) string {
	if item.Season > 0 || item.Episode > 0 {
		return "show"
	}
	// Check filename for season/episode patterns
	if seasonRegex.MatchString(item.FileName) || altEpRegex.MatchString(item.FileName) || seasonOnly.MatchString(item.FileName) {
		return "show"
	}
	return "movie"
}

// ParseSeasonEpisode extracts season and episode numbers from a filename.
func ParseSeasonEpisode(filename string) (season, episode int) {
	// S01E05
	if m := seasonRegex.FindStringSubmatch(filename); len(m) > 2 {
		season = atoi(m[1])
		episode = atoi(m[2])
		return
	}
	// 1x05
	if m := altEpRegex.FindStringSubmatch(filename); len(m) > 2 {
		season = atoi(m[1])
		episode = atoi(m[2])
		return
	}
	// S01 only (season pack)
	if m := seasonOnly.FindStringSubmatch(filename); len(m) > 1 {
		season = atoi(m[1])
		return
	}
	return 0, 0
}

// PrimaryAudioTrack returns the codec and channel count of the default or first audio track.
func PrimaryAudioTrack(tracks []mediainfo.AudioTrack) (codec string, channels int) {
	if len(tracks) == 0 {
		return "", 0
	}
	for _, t := range tracks {
		if t.Default {
			return t.Codec, t.Channels
		}
	}
	return tracks[0].Codec, tracks[0].Channels
}

// AudioLanguages extracts unique language codes from audio tracks.
func AudioLanguages(tracks []mediainfo.AudioTrack) []string {
	return mediainfo.ComputeLanguages(tracks)
}

// SubtitleLanguages extracts unique language codes from subtitle tracks.
func SubtitleLanguages(tracks []mediainfo.SubtitleTrack) []string {
	seen := make(map[string]struct{})
	for _, t := range tracks {
		if t.Lang != "" && t.Lang != "und" {
			seen[t.Lang] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for l := range seen {
		result = append(result, l)
	}
	return result
}

// CleanTitle extracts a clean title from a filename for searching.
// Removes extension, replaces separators with spaces, strips release artifacts.
func CleanTitle(filename string) string {
	// Remove extension
	name := strings.TrimSuffix(filename, extOf(filename))

	// Remove release group at end BEFORE replacing separators (e.g. "-SPARKS", "-FGT")
	name = regexp.MustCompile(`-[A-Za-z0-9]+$`).ReplaceAllString(name, "")

	// Remove brackets
	name = regexp.MustCompile(`[\[\(].*?[\]\)]`).ReplaceAllString(name, "")

	// Remove web domains BEFORE replacing separators (dots are still dots here)
	name = regexp.MustCompile(`(?i)[a-z0-9]+\.(com|org|net|mx|io|to|cc|se)`).ReplaceAllString(name, "")

	// Replace common separators with spaces
	name = strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(name)

	// Remove quality/codec/release artifacts
	name = regexp.MustCompile(`(?i)\b(2160p|1080p|720p|480p|4K|UHD|BluRay|BDRip|WEBRip|WEB-DL|HDTV|DVDRip|BRRip|x264|x265|HEVC|AVC|AV1|AAC|DTS|AC3|Atmos|FLAC|10bit|HDR10?\+?|DV|DoVi|PROPER|REPACK|REMUX|EXTENDED|DUAL|MULTi|UHDremux|4Kremux\d*)\b`).ReplaceAllString(name, "")

	// Remove standalone numbers that look like resolution/format (e.g. "2160", "1080")
	name = regexp.MustCompile(`\b(2160|1080|720|480)\b`).ReplaceAllString(name, "")

	// Remove year
	name = regexp.MustCompile(`\b(19|20)\d{2}\b`).ReplaceAllString(name, "")

	// Collapse whitespace
	name = regexp.MustCompile(`\s+`).ReplaceAllString(name, " ")
	return strings.TrimSpace(name)
}

func extOf(filename string) string {
	for i := len(filename) - 1; i >= 0; i-- {
		if filename[i] == '.' {
			return filename[i:]
		}
	}
	return ""
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}

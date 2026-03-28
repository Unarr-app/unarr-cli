package parser

import (
	"net/url"
	"regexp"
	"strings"
)

// ParsedTorrent contains information extracted from a magnet URI, hash, or torrent name.
type ParsedTorrent struct {
	InfoHash string
	Name     string
	Quality  string
	Codec    string
	Year     string
	IsMagnet bool
}

var (
	hashRegex       = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)
	qualityRegex    = regexp.MustCompile(`(?i)(2160p|1080p|720p|480p|4K|UHD)`)
	codecRegex      = regexp.MustCompile(`(?i)(x264|x265|h\.?264|h\.?265|HEVC|AVC|AV1|VP9|XviD|DivX)`)
	yearRegex       = regexp.MustCompile(`(?:^|[\s.(])((?:19|20)\d{2})(?:[\s.)]|$)`)
	artifactsRegex  = regexp.MustCompile(`(?i)(BluRay|BDRip|HDRip|WEBRip|WEB-DL|HDTV|DVDRip|BRRip|CAM|TS|TC|PROPER|REPACK|REMASTERED|REMUX|EXTENDED|UNRATED|IMAX|DUAL|MULTi|AAC|DTS|DD5\.1|AC3|Atmos|FLAC|EAC3|10bit|HDR10?\+?|DV|DoVi|SDR|YTS|YIFY|RARBG|NTG|SPARKS|AMIABLE|FGT|\[.*?\]|\(.*?\))`)
	whitespaceRegex = regexp.MustCompile(`\s+`)
)

// Parse parses a magnet URI, info hash, or torrent name.
func Parse(input string) ParsedTorrent {
	input = strings.TrimSpace(input)

	if strings.HasPrefix(input, "magnet:") {
		return parseMagnet(input)
	}

	if hashRegex.MatchString(input) {
		return ParsedTorrent{
			InfoHash: strings.ToLower(input),
		}
	}

	// Treat as a torrent name/filename
	return parseName(input)
}

func parseMagnet(uri string) ParsedTorrent {
	result := ParsedTorrent{IsMagnet: true}

	u, err := url.Parse(uri)
	if err != nil {
		return result
	}

	xt := u.Query().Get("xt")
	if strings.HasPrefix(xt, "urn:btih:") {
		result.InfoHash = strings.ToLower(strings.TrimPrefix(xt, "urn:btih:"))
	}

	dn := u.Query().Get("dn")
	if dn != "" {
		result.Name = dn
		parsed := parseName(dn)
		result.Quality = parsed.Quality
		result.Codec = parsed.Codec
		result.Year = parsed.Year
	}

	return result
}

func parseName(name string) ParsedTorrent {
	result := ParsedTorrent{Name: name}

	if m := qualityRegex.FindString(name); m != "" {
		result.Quality = strings.ToLower(m)
		if result.Quality == "4k" || result.Quality == "uhd" {
			result.Quality = "2160p"
		}
	}

	if m := codecRegex.FindString(name); m != "" {
		result.Codec = m
	}

	if m := yearRegex.FindStringSubmatch(name); len(m) > 1 {
		result.Year = m[1]
	}

	return result
}

// ExtractSearchQuery cleans a torrent name to use as a search query.
func ExtractSearchQuery(name string) string {
	q := name

	// Remove common release artifacts
	for _, re := range []*regexp.Regexp{qualityRegex, codecRegex} {
		q = re.ReplaceAllString(q, "")
	}

	q = artifactsRegex.ReplaceAllString(q, "")

	// Replace dots and underscores with spaces
	q = strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(q)

	// Remove year
	q = yearRegex.ReplaceAllString(q, " ")

	// Collapse whitespace
	q = whitespaceRegex.ReplaceAllString(q, " ")
	q = strings.TrimSpace(q)

	return q
}

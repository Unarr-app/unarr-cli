package library

import (
	"regexp"
	"strings"
	"testing"
)

// Differential guard: the 3-4 digit episode reading and the {…} tag stripping
// must not change how any other release name is read. legacy* are the v1.15.4
// implementations, copied verbatim.

var legacySeasonRegex = regexp.MustCompile(`(?i)S(\d{1,2})E(\d{1,2})`)

func legacyParseSeasonEpisode(filename string) (season, episode int) {
	if m := legacySeasonRegex.FindStringSubmatch(filename); len(m) > 2 {
		return atoi(m[1]), atoi(m[2])
	}
	if m := altEpRegex.FindStringSubmatch(filename); len(m) > 2 {
		return atoi(m[1]), atoi(m[2])
	}
	if m := seasonOnly.FindStringSubmatch(filename); len(m) > 1 {
		return atoi(m[1]), 0
	}
	return 0, 0
}

var releaseCorpus = []string{
	"Breaking.Bad.S01E05.720p.HDTV.x264-CTU.mkv",
	"The.Office.US.S09E23E24.1080p.WEB-DL.mkv",
	"Show.S01E05-E06.1080p.mkv",
	"Show.S01E05E06E07.mkv",
	"show.s3e7.mkv",
	"Show S02 E05 1080p.mkv",
	"Show.S02.COMPLETE.1080p.BluRay.mkv",
	"Show.Season.2.1080p.mkv",
	"Show.2x05.HDTV.avi",
	"Show.12x03.mkv",
	"Show.S10E100.Special.mkv",
	"[SubsPlease] Frieren - 03 (1080p) [ABCD1234].mkv",
	"One.Piece.S01E01720p.mkv",
	"Show.S01E12345.mkv",
	"Show.S01E99999.S02E03.mkv",
	"Movie.2019.1080p.BluRay.x264-SPARKS.mkv",
	"Inception (2010) [1080p].mkv",
	"S01E01.mkv",
	"s1e1.mp4",
	"Show.S01E01.1080p.x265-RARBG",
	"Dexter.S08E12.Remember.the.Monsters.1080p.mkv",
	"Mr.Robot.S04E13.eps4.11_whoami.mkv",
	"Doctor.Who.2005.S13E06.mkv",
	"Show (2019) - S01E05 - Title.mkv",
	"Show.S01E05.Part.2.mkv",
	"MASH.S11E16.Goodbye.Farewell.and.Amen.mkv",
	"9-1-1.S07E10.1080p.mkv",
	"Show.S2024E01.mkv",
	"Show.S01EP05.mkv",
}

func TestParseSeasonEpisodeParityWithLegacy(t *testing.T) {
	long := []struct {
		name    string
		episode int
	}{
		{"One.Piece.S22E1089.1080p.mkv", 1089},
		{"One Piece (1999) - S01E0001 - Title.mkv", 1},
		{"Detective.Conan.S01E1100.mkv", 1100},
		{"Show.S10E100.Special.mkv", 100},
	}
	isLong := map[string]int{}
	for _, l := range long {
		isLong[l.name] = l.episode
		if _, e := ParseSeasonEpisode(l.name); e != l.episode {
			t.Errorf("ParseSeasonEpisode(%q) episode = %d, want %d", l.name, e, l.episode)
		}
	}
	for _, name := range releaseCorpus {
		if _, ok := isLong[name]; ok {
			continue
		}
		gs, ge := ParseSeasonEpisode(name)
		ws, we := legacyParseSeasonEpisode(name)
		if gs != ws || ge != we {
			t.Errorf("ParseSeasonEpisode(%q) = (%d,%d), legacy (%d,%d)", name, gs, ge, ws, we)
		}
		if got, want := DeriveContentType(LibraryItem{FileName: name}), legacyContentType(name); got != want {
			t.Errorf("DeriveContentType(%q) = %s, legacy %s", name, got, want)
		}
	}
}

func legacyContentType(name string) string {
	if legacySeasonRegex.MatchString(name) || altEpRegex.MatchString(name) || seasonOnly.MatchString(name) {
		return "show"
	}
	return "movie"
}

func TestCleanTitleParityWithLegacy(t *testing.T) {
	for _, name := range releaseCorpus {
		if strings.Contains(name, "{") {
			continue
		}
		if got, want := CleanTitle(name), legacyCleanTitle(name); got != want {
			t.Errorf("CleanTitle(%q) = %q, legacy %q", name, got, want)
		}
	}
}

func legacyCleanTitle(filename string) string {
	name := strings.TrimSuffix(filename, extOf(filename))
	name = regexp.MustCompile(`-[A-Za-z0-9]+$`).ReplaceAllString(name, "")
	name = regexp.MustCompile(`[\[\(].*?[\]\)]`).ReplaceAllString(name, "")
	name = regexp.MustCompile(`(?i)[a-z0-9]+\.(com|org|net|mx|io|to|cc|se)`).ReplaceAllString(name, "")
	name = strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(name)
	name = regexp.MustCompile(`(?i)\b(2160p|1080p|720p|480p|4K|UHD|BluRay|BDRip|WEBRip|WEB-DL|HDTV|DVDRip|BRRip|x264|x265|HEVC|AVC|AV1|AAC|DTS|AC3|Atmos|FLAC|10bit|HDR10?\+?|DV|DoVi|PROPER|REPACK|REMUX|EXTENDED|DUAL|MULTi|UHDremux|4Kremux\d*)\b`).ReplaceAllString(name, "")
	name = regexp.MustCompile(`\b(2160|1080|720|480)\b`).ReplaceAllString(name, "")
	name = regexp.MustCompile(`\b(19|20)\d{2}\b`).ReplaceAllString(name, "")
	name = regexp.MustCompile(`\s+`).ReplaceAllString(name, " ")
	return strings.TrimSpace(name)
}

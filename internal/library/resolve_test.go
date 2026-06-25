package library

import (
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/library/mediainfo"
)

func TestResolveResolution(t *testing.T) {
	tests := []struct {
		name   string
		width  int
		height int
		want   string
	}{
		{"4K square", 3840, 2160, "2160p"},
		{"4K low height", 3840, 1600, "2160p"},
		{"1080p square", 1920, 1080, "1080p"},
		{"1080p cinematic 2.39:1", 1920, 804, "1080p"}, // anamorphic widescreen — must not fall to 720p
		{"1080p cinematic 2.35:1", 1920, 818, "1080p"},
		{"1080p 21:9", 2560, 1080, "1080p"},
		{"720p square", 1280, 720, "720p"},
		{"720p widescreen", 1280, 540, "720p"},
		{"480p", 854, 480, "480p"},
		{"sub-480", 640, 360, ""},
		{"zero", 0, 0, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveResolution(tt.width, tt.height)
			if got != tt.want {
				t.Errorf("ResolveResolution(%d, %d) = %q, want %q", tt.width, tt.height, got, tt.want)
			}
		})
	}
}

func TestDeriveContentType(t *testing.T) {
	tests := []struct {
		name string
		item LibraryItem
		want string
	}{
		{
			"movie by default",
			LibraryItem{FileName: "Inception.2010.1080p.mkv"},
			"movie",
		},
		{
			"show by season field",
			LibraryItem{FileName: "something.mkv", Season: 1},
			"show",
		},
		{
			"show by episode field",
			LibraryItem{FileName: "something.mkv", Episode: 5},
			"show",
		},
		{
			"show by S01E01 in filename",
			LibraryItem{FileName: "Breaking.Bad.S01E01.1080p.mkv"},
			"show",
		},
		{
			"show by 1x05 in filename",
			LibraryItem{FileName: "show.1x05.720p.mkv"},
			"show",
		},
		{
			"show by S02 in filename",
			LibraryItem{FileName: "Show.Name.S02.Complete.mkv"},
			"show",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DeriveContentType(tt.item)
			if got != tt.want {
				t.Errorf("DeriveContentType() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseSeasonEpisode(t *testing.T) {
	tests := []struct {
		filename string
		season   int
		episode  int
	}{
		{"Breaking.Bad.S01E05.1080p.mkv", 1, 5},
		{"Show.S02E10.720p.mkv", 2, 10},
		{"show.1x05.mkv", 1, 5},
		{"show.12x03.mkv", 12, 3},
		{"Show.S01.Complete.mkv", 1, 0},
		{"Inception.2010.1080p.mkv", 0, 0},
		{"s3e7.mkv", 3, 7},
	}

	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			s, e := ParseSeasonEpisode(tt.filename)
			if s != tt.season || e != tt.episode {
				t.Errorf("ParseSeasonEpisode(%q) = (%d, %d), want (%d, %d)", tt.filename, s, e, tt.season, tt.episode)
			}
		})
	}
}

func TestPrimaryAudioTrack(t *testing.T) {
	// Default track
	tracks := []mediainfo.AudioTrack{
		{Lang: "en", Codec: "aac", Channels: 2, Default: false},
		{Lang: "es", Codec: "ac3", Channels: 6, Default: true},
	}
	codec, ch := PrimaryAudioTrack(tracks)
	if codec != "ac3" || ch != 6 {
		t.Errorf("expected ac3/6, got %s/%d", codec, ch)
	}

	// No default → first
	tracks2 := []mediainfo.AudioTrack{
		{Lang: "en", Codec: "dts", Channels: 8},
		{Lang: "es", Codec: "aac", Channels: 2},
	}
	codec, ch = PrimaryAudioTrack(tracks2)
	if codec != "dts" || ch != 8 {
		t.Errorf("expected dts/8, got %s/%d", codec, ch)
	}

	// Empty
	codec, ch = PrimaryAudioTrack(nil)
	if codec != "" || ch != 0 {
		t.Errorf("expected empty, got %s/%d", codec, ch)
	}
}

func TestCleanTitle(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Inception.2010.1080p.BluRay.x264-SPARKS.mkv", "Inception"},
		{"Breaking.Bad.S01E05.720p.HDTV.mkv", "Breaking Bad S01E05"},
		{"The.Matrix.1999.2160p.UHD.BluRay.REMUX.mkv", "The Matrix"},
		{"Movie [YTS.MX].mp4", "Movie"},
		{"Greenland 4Kremux2160.pctfenix.com.mkv", "Greenland"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := CleanTitle(tt.input)
			if got != tt.want {
				t.Errorf("CleanTitle(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

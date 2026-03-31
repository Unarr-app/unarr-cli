package mediainfo

import (
	"testing"
)

func TestParseDuration(t *testing.T) {
	tests := []struct {
		input string
		want  float64
	}{
		{"", 0},
		{"0", 0},
		{"-5", 0},
		{"invalid", 0},
		{"7423.500000", 7423.5},
		{"120.123456", 120.123},
		{"3600", 3600},
		{"0.001", 0.001},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseDuration(tt.input)
			if got != tt.want {
				t.Errorf("parseDuration(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestTagValue(t *testing.T) {
	tags := map[string]string{
		"language": "eng",
		"title":    "Main Audio",
		"HANDLER":  "VideoHandler",
	}

	tests := []struct {
		key  string
		want string
	}{
		{"language", "eng"},
		{"title", "Main Audio"},
		{"handler", "VideoHandler"},
		{"missing", ""},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got := tagValue(tags, tt.key)
			if got != tt.want {
				t.Errorf("tagValue(tags, %q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestTagValueNil(t *testing.T) {
	got := tagValue(nil, "language")
	if got != "" {
		t.Errorf("tagValue(nil, language) = %q, want empty", got)
	}
}

func TestContainsAny(t *testing.T) {
	tests := []struct {
		s    string
		subs []string
		want bool
	}{
		{"yuv420p10le", []string{"10le", "10be", "p010"}, true},
		{"yuv420p12be", []string{"10le", "10be", "p010"}, false},
		{"yuv420p12be", []string{"12le", "12be"}, true},
		{"yuv420p", []string{"10le", "10be"}, false},
		{"", []string{"any"}, false},
		{"something", []string{}, false},
	}

	for _, tt := range tests {
		got := containsAny(tt.s, tt.subs...)
		if got != tt.want {
			t.Errorf("containsAny(%q, %v) = %v, want %v", tt.s, tt.subs, got, tt.want)
		}
	}
}

func TestParseFFprobeOutput_BasicH264(t *testing.T) {
	data := ffprobeOutput{
		Format: ffprobeFormat{Duration: "7423.5"},
		Streams: []ffprobeStream{
			{
				CodecType:  "video",
				CodecName:  "h264",
				Profile:    "High",
				Width:      1920,
				Height:     1080,
				RFrameRate: "24000/1001",
			},
			{
				CodecType:   "audio",
				CodecName:   "aac",
				Channels:    2,
				Tags:        map[string]string{"language": "eng"},
				Disposition: map[string]int{"default": 1},
			},
		},
	}

	mi, err := parseFFprobeOutput(data)
	if err != nil {
		t.Fatalf("parseFFprobeOutput: %v", err)
	}
	if mi.Video == nil {
		t.Fatal("expected video info")
	}
	if mi.Video.Codec != "h264" {
		t.Errorf("codec = %q, want h264", mi.Video.Codec)
	}
	if mi.Video.Width != 1920 || mi.Video.Height != 1080 {
		t.Errorf("dimensions = %dx%d, want 1920x1080", mi.Video.Width, mi.Video.Height)
	}
	if mi.Video.Profile != "High" {
		t.Errorf("profile = %q, want High", mi.Video.Profile)
	}
	if mi.Video.Duration != 7423.5 {
		t.Errorf("duration = %v, want 7423.5", mi.Video.Duration)
	}
	if mi.Video.FrameRate < 23.975 || mi.Video.FrameRate > 23.977 {
		t.Errorf("frameRate = %v, want ~23.976", mi.Video.FrameRate)
	}
	if len(mi.Audio) != 1 {
		t.Fatalf("audio tracks = %d, want 1", len(mi.Audio))
	}
	if mi.Audio[0].Lang != "en" {
		t.Errorf("audio lang = %q, want en", mi.Audio[0].Lang)
	}
	if !mi.Audio[0].Default {
		t.Error("expected default audio track")
	}
}

func TestParseFFprobeOutput_HEVC_HDR10(t *testing.T) {
	data := ffprobeOutput{
		Format: ffprobeFormat{Duration: "3600"},
		Streams: []ffprobeStream{
			{
				CodecType:     "video",
				CodecName:     "hevc",
				Width:         3840,
				Height:        2160,
				BitsPerRaw:    "10",
				ColorSpace:    "bt2020nc",
				ColorTransfer: "smpte2084",
				RFrameRate:    "24/1",
			},
		},
	}

	mi, err := parseFFprobeOutput(data)
	if err != nil {
		t.Fatal(err)
	}
	if mi.Video.HDR != "HDR10" {
		t.Errorf("hdr = %q, want HDR10", mi.Video.HDR)
	}
	if mi.Video.BitDepth != 10 {
		t.Errorf("bitDepth = %d, want 10", mi.Video.BitDepth)
	}
}

func TestParseFFprobeOutput_DolbyVisionWithHDR10(t *testing.T) {
	data := ffprobeOutput{
		Streams: []ffprobeStream{
			{
				CodecType:     "video",
				CodecName:     "hevc",
				Width:         3840,
				Height:        2160,
				ColorSpace:    "bt2020nc",
				ColorTransfer: "smpte2084",
				SideDataList:  []sideData{{SideDataType: "DOVI configuration record"}},
			},
		},
	}

	mi, err := parseFFprobeOutput(data)
	if err != nil {
		t.Fatal(err)
	}
	if mi.Video.HDR != "DV+HDR10" {
		t.Errorf("hdr = %q, want DV+HDR10", mi.Video.HDR)
	}
}

func TestParseFFprobeOutput_DolbyVisionOnly(t *testing.T) {
	data := ffprobeOutput{
		Streams: []ffprobeStream{
			{
				CodecType:    "video",
				CodecName:    "hevc",
				Width:        3840,
				Height:       2160,
				SideDataList: []sideData{{SideDataType: "DOVI configuration record"}},
			},
		},
	}

	mi, err := parseFFprobeOutput(data)
	if err != nil {
		t.Fatal(err)
	}
	if mi.Video.HDR != "DV" {
		t.Errorf("hdr = %q, want DV", mi.Video.HDR)
	}
}

func TestParseFFprobeOutput_HLG(t *testing.T) {
	data := ffprobeOutput{
		Streams: []ffprobeStream{
			{
				CodecType:     "video",
				CodecName:     "hevc",
				Width:         3840,
				Height:        2160,
				ColorSpace:    "bt2020nc",
				ColorTransfer: "arib-std-b67",
			},
		},
	}

	mi, err := parseFFprobeOutput(data)
	if err != nil {
		t.Fatal(err)
	}
	if mi.Video.HDR != "HLG" {
		t.Errorf("hdr = %q, want HLG", mi.Video.HDR)
	}
}

func TestParseFFprobeOutput_MultiAudioAndSubtitles(t *testing.T) {
	data := ffprobeOutput{
		Format: ffprobeFormat{Duration: "5400"},
		Streams: []ffprobeStream{
			{CodecType: "video", CodecName: "h264", Width: 1920, Height: 1080},
			{
				CodecType: "audio", CodecName: "ac3", Channels: 6,
				Tags:        map[string]string{"language": "eng", "title": "English 5.1"},
				Disposition: map[string]int{"default": 1},
			},
			{
				CodecType: "audio", CodecName: "aac", Channels: 2,
				Tags: map[string]string{"language": "spa"},
			},
			{
				CodecType: "subtitle", CodecName: "subrip",
				Tags: map[string]string{"language": "eng"},
			},
			{
				CodecType: "subtitle", CodecName: "ass",
				Tags:        map[string]string{"language": "spa"},
				Disposition: map[string]int{"forced": 1},
			},
		},
	}

	mi, err := parseFFprobeOutput(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(mi.Audio) != 2 {
		t.Fatalf("audio tracks = %d, want 2", len(mi.Audio))
	}
	if mi.Audio[0].Title != "English 5.1" {
		t.Errorf("audio[0].title = %q", mi.Audio[0].Title)
	}
	if len(mi.Subtitles) != 2 {
		t.Fatalf("subtitle tracks = %d, want 2", len(mi.Subtitles))
	}
	if !mi.Subtitles[1].Forced {
		t.Error("expected subtitle[1] to be forced")
	}
	if len(mi.Languages) != 2 {
		t.Errorf("languages = %v, want 2 entries", mi.Languages)
	}
}

func TestParseFFprobeOutput_BitDepthFromPixFmt(t *testing.T) {
	data := ffprobeOutput{
		Streams: []ffprobeStream{
			{CodecType: "video", CodecName: "hevc", Width: 1920, Height: 1080, PixFmt: "yuv420p10le"},
		},
	}

	mi, err := parseFFprobeOutput(data)
	if err != nil {
		t.Fatal(err)
	}
	if mi.Video.BitDepth != 10 {
		t.Errorf("bitDepth = %d, want 10", mi.Video.BitDepth)
	}
}

func TestParseFFprobeOutput_12BitFromPixFmt(t *testing.T) {
	data := ffprobeOutput{
		Streams: []ffprobeStream{
			{CodecType: "video", CodecName: "hevc", Width: 1920, Height: 1080, PixFmt: "yuv420p12be"},
		},
	}

	mi, err := parseFFprobeOutput(data)
	if err != nil {
		t.Fatal(err)
	}
	if mi.Video.BitDepth != 12 {
		t.Errorf("bitDepth = %d, want 12", mi.Video.BitDepth)
	}
}

func TestParseFFprobeOutput_DurationFromStreamFallback(t *testing.T) {
	data := ffprobeOutput{
		Format: ffprobeFormat{Duration: ""},
		Streams: []ffprobeStream{
			{CodecType: "video", CodecName: "h264", Width: 1280, Height: 720, Duration: "1800.5"},
		},
	}

	mi, err := parseFFprobeOutput(data)
	if err != nil {
		t.Fatal(err)
	}
	if mi.Video.Duration != 1800.5 {
		t.Errorf("duration = %v, want 1800.5", mi.Video.Duration)
	}
}

func TestParseFFprobeOutput_NoStreams(t *testing.T) {
	data := ffprobeOutput{}
	_, err := parseFFprobeOutput(data)
	if err == nil {
		t.Error("expected error for no streams")
	}
}

func TestParseFFprobeOutput_OnlyFirstVideoStream(t *testing.T) {
	data := ffprobeOutput{
		Streams: []ffprobeStream{
			{CodecType: "video", CodecName: "h264", Width: 1920, Height: 1080},
			{CodecType: "video", CodecName: "mjpeg", Width: 320, Height: 240}, // cover art
		},
	}

	mi, err := parseFFprobeOutput(data)
	if err != nil {
		t.Fatal(err)
	}
	if mi.Video.Codec != "h264" {
		t.Errorf("should use first video stream, got codec %q", mi.Video.Codec)
	}
	if mi.Video.Width != 1920 {
		t.Errorf("width = %d, should be from first video stream", mi.Video.Width)
	}
}

func TestParseFFprobeOutput_SMPTE2084_WithoutBT2020(t *testing.T) {
	data := ffprobeOutput{
		Streams: []ffprobeStream{
			{CodecType: "video", CodecName: "hevc", Width: 3840, Height: 2160, ColorTransfer: "smpte2084"},
		},
	}

	mi, err := parseFFprobeOutput(data)
	if err != nil {
		t.Fatal(err)
	}
	if mi.Video.HDR != "HDR10" {
		t.Errorf("hdr = %q, want HDR10", mi.Video.HDR)
	}
}

func TestParseFFprobeOutput_AribWithoutBT2020(t *testing.T) {
	data := ffprobeOutput{
		Streams: []ffprobeStream{
			{CodecType: "video", CodecName: "hevc", Width: 3840, Height: 2160, ColorTransfer: "arib-std-b67", ColorSpace: "other"},
		},
	}

	mi, err := parseFFprobeOutput(data)
	if err != nil {
		t.Fatal(err)
	}
	if mi.Video.HDR != "HLG" {
		t.Errorf("hdr = %q, want HLG", mi.Video.HDR)
	}
}

func TestParseFFprobeOutput_AudioOnly(t *testing.T) {
	data := ffprobeOutput{
		Streams: []ffprobeStream{
			{CodecType: "audio", CodecName: "flac", Channels: 2, Tags: map[string]string{"language": "eng"}},
		},
	}

	mi, err := parseFFprobeOutput(data)
	if err != nil {
		t.Fatal(err)
	}
	if mi.Video != nil {
		t.Error("expected no video info for audio-only")
	}
	if len(mi.Audio) != 1 {
		t.Errorf("audio tracks = %d, want 1", len(mi.Audio))
	}
}

func TestParseFFprobeOutput_FrameRateNoSlash(t *testing.T) {
	data := ffprobeOutput{
		Streams: []ffprobeStream{
			{CodecType: "video", CodecName: "h264", Width: 1920, Height: 1080, RFrameRate: "30"},
		},
	}

	mi, err := parseFFprobeOutput(data)
	if err != nil {
		t.Fatal(err)
	}
	if mi.Video.FrameRate != 0 {
		t.Errorf("frameRate = %v, want 0 (no slash)", mi.Video.FrameRate)
	}
}

package engine

import "testing"

func TestDecideAction(t *testing.T) {
	cases := []struct {
		name string
		p    StreamProbe
		want TranscodeAction
	}{
		{
			name: "MP4 + h264 + AAC = passthrough",
			p:    StreamProbe{VideoCodec: "h264", AudioCodec: "aac", Container: ".mp4"},
			want: ActionPassthrough,
		},
		{
			name: "MKV + h264 + AAC = remux",
			p:    StreamProbe{VideoCodec: "h264", AudioCodec: "aac", Container: ".mkv"},
			want: ActionRemux,
		},
		{
			name: "MKV + h264 + AC3 = remux audio",
			p:    StreamProbe{VideoCodec: "h264", AudioCodec: "ac3", Container: ".mkv"},
			want: ActionRemuxAudio,
		},
		{
			name: "MP4 + h264 + EAC3 = remux audio",
			p:    StreamProbe{VideoCodec: "h264", AudioCodec: "eac3", Container: ".mp4"},
			want: ActionRemuxAudio,
		},
		{
			name: "MKV + HEVC = transcode video",
			p:    StreamProbe{VideoCodec: "hevc", AudioCodec: "aac", Container: ".mkv"},
			want: ActionTranscodeVideo,
		},
		{
			name: "MP4 + AV1 = transcode video",
			p:    StreamProbe{VideoCodec: "av1", AudioCodec: "aac", Container: ".mp4"},
			want: ActionTranscodeVideo,
		},
		{
			name: "h264 10-bit = transcode video (browser refuses)",
			p:    StreamProbe{VideoCodec: "h264", AudioCodec: "aac", BitDepth: 10, Container: ".mp4"},
			want: ActionTranscodeVideo,
		},
		{
			name: "h264 + HDR10 = transcode video",
			p:    StreamProbe{VideoCodec: "h264", AudioCodec: "aac", HDR: "HDR10", Container: ".mp4"},
			want: ActionTranscodeVideo,
		},
		{
			name: "AVI + h264 + AAC = remux",
			p:    StreamProbe{VideoCodec: "h264", AudioCodec: "aac", Container: ".avi"},
			want: ActionRemux,
		},
		{
			name: "Unknown codec = transcode video",
			p:    StreamProbe{VideoCodec: "mpeg4", AudioCodec: "mp3", Container: ".avi"},
			want: ActionTranscodeVideo,
		},
		{
			name: "Empty probe falls through to transcode (unknown codec)",
			p:    StreamProbe{},
			want: ActionTranscodeVideo,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideAction(&tc.p)
			if got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestDecideActionNil(t *testing.T) {
	if DecideAction(nil) != ActionPassthrough {
		t.Error("nil probe should default passthrough")
	}
}

func TestLowerExt(t *testing.T) {
	cases := map[string]string{
		"foo.MP4":              ".mp4",
		"path/to/movie.MKV":    ".mkv",
		"weird.name.with.dots": ".dots",
		"":                     "",
		"noext":                "",
	}
	for in, want := range cases {
		if got := lowerExt(in); got != want {
			t.Errorf("lowerExt(%q) = %q want %q", in, got, want)
		}
	}
}

package mediainfo

import (
	"strings"
	"testing"
)

func TestFilterVTTDrawingCues(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		comment string
	}{
		{
			name: "drops a pure drawing cue",
			// Verbatim from Exiled Heavy Knight S01E01 at 18:37 — the cue that
			// paints "m 0 0 l 290 0 290 42 0 42" over the picture today.
			in: "WEBVTT\n\n" +
				"18:37.850 --> 18:43.520\nRampart Reflection\n\n" +
				"18:37.850 --> 18:43.520\nm 0 0 l 290 0 290 42 0 42\n\n" +
				"18:37.850 --> 18:43.520\nCommon Skill\n",
			want: "WEBVTT\n\n" +
				"18:37.850 --> 18:43.520\nRampart Reflection\n\n" +
				"18:37.850 --> 18:43.520\nCommon Skill\n",
			comment: "the sign's caption cues share the timestamp and must survive",
		},
		{
			name:    "keeps dialogue that merely starts like a path",
			in:      "WEBVTT\n\n00:01.000 --> 00:02.000\nm 3 kg de arroz\n",
			want:    "WEBVTT\n\n00:01.000 --> 00:02.000\nm 3 kg de arroz\n",
			comment: "guard 1 passes, guard 2 (closed vocabulary) rejects it",
		},
		{
			name:    "keeps dialogue with numbers and commas",
			in:      "WEBVTT\n\n00:01.000 --> 00:02.000\n1, 2, 3... go!\n",
			want:    "WEBVTT\n\n00:01.000 --> 00:02.000\n1, 2, 3... go!\n",
			comment: "no leading move command",
		},
		{
			name: "drops multi-line drawing, keeps mixed cue",
			in: "WEBVTT\n\n" +
				"00:01.000 --> 00:02.000\nm 0 0 l 10 0\nm 5 5 b 1 2 3 4 5 6\n\n" +
				"00:03.000 --> 00:04.000\nm 0 0 l 10 0\nActual caption\n",
			want: "WEBVTT\n\n" +
				"00:03.000 --> 00:04.000\nm 0 0 l 10 0\nActual caption\n",
			comment: "a cue is dropped only when it has NO renderable text",
		},
		{
			name: "preserves NOTE and STYLE blocks",
			in: "WEBVTT\n\n" +
				"NOTE this is a comment\n\n" +
				"STYLE\n::cue { color: peachpuff; }\n\n" +
				"00:01.000 --> 00:02.000\nm 0 0 l 10 0\n\n" +
				"00:03.000 --> 00:04.000\nHello\n",
			want: "WEBVTT\n\n" +
				"NOTE this is a comment\n\n" +
				"STYLE\n::cue { color: peachpuff; }\n\n" +
				"00:03.000 --> 00:04.000\nHello\n",
			comment: "blocks without a timing line are never candidates",
		},
		{
			name:    "keeps cue identifiers and settings",
			in:      "WEBVTT\n\nsign-1\n00:01.000 --> 00:02.000 line:90% align:start\nHello\n",
			want:    "WEBVTT\n\nsign-1\n00:01.000 --> 00:02.000 line:90% align:start\nHello\n",
			comment: "the timing line is located, not assumed to be first",
		},
		{
			name:    "all cues dropped still leaves a valid header",
			in:      "WEBVTT\n\n00:01.000 --> 00:02.000\nm 0 0 l 10 0\n",
			want:    "WEBVTT",
			comment: "a header-only VTT loads cleanly and renders nothing",
		},
		{
			name:    "input without cues is returned untouched",
			in:      "WEBVTT\n\nNOTE nothing here\n",
			want:    "WEBVTT\n\nNOTE nothing here\n",
			comment: "no '-->' anywhere → early return",
		},
		{
			name:    "negative and decimal coordinates are recognised",
			in:      "WEBVTT\n\n00:01.000 --> 00:02.000\nm -12.5 0 l 290.75 -42\n\n00:03.000 --> 00:04.000\nHi\n",
			want:    "WEBVTT\n\n00:03.000 --> 00:04.000\nHi\n",
			comment: "signs are drawn at fractional positions after \\pos scaling",
		},
		{
			name:    "empty input",
			in:      "",
			want:    "",
			comment: "guard against a zero-byte upstream body",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := string(FilterVTTDrawingCues([]byte(tc.in)))
			if got != tc.want {
				t.Errorf("%s\n got: %q\nwant: %q", tc.comment, got, tc.want)
			}
		})
	}
}

func TestFilterVTTDrawingCuesPreservesCRLF(t *testing.T) {
	in := "WEBVTT\r\n\r\n00:01.000 --> 00:02.000\r\nm 0 0 l 10 0\r\n\r\n00:03.000 --> 00:04.000\r\nHello\r\n"
	got := string(FilterVTTDrawingCues([]byte(in)))
	if strings.Contains(got, "m 0 0 l 10 0") {
		t.Fatalf("drawing cue survived CRLF input: %q", got)
	}
	if !strings.Contains(got, "\r\n") {
		t.Errorf("CRLF line endings were not preserved: %q", got)
	}
}

func TestFilterVTTDrawingCuesUnchangedWhenClean(t *testing.T) {
	// The common case: a plain dialogue track. The function must hand back the
	// SAME backing array so the hot path costs nothing but a scan.
	in := []byte("WEBVTT\n\n00:01.000 --> 00:02.000\nJust dialogue.\n")
	got := FilterVTTDrawingCues(in)
	if &got[0] != &in[0] {
		t.Error("clean input was copied instead of passed through")
	}
}

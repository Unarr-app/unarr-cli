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

// Regressions from the code review — each of these was a real defect.
func TestFilterVTTDrawingCuesReviewRegressions(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		comment string
	}{
		{
			name:    "malformed file with no blank line after the header keeps its signature",
			in:      "WEBVTT\n00:01.000 --> 00:02.000\nm 0 0 l 5 5\n",
			want:    "WEBVTT",
			comment: "dropping that block returned \"\" — a body with no WEBVTT signature, which every browser rejects",
		},
		{
			name:    "short numeric caption is not a drawing",
			in:      "WEBVTT\n\n00:01.000 --> 00:02.000\nm 2 3, 4\n",
			want:    "WEBVTT\n\n00:01.000 --> 00:02.000\nm 2 3, 4\n",
			comment: "opens like a move command and uses only allowed characters, but has no path command — a real drawing always does",
		},
		{
			name:    "a move command alone is not a drawing",
			in:      "WEBVTT\n\n00:01.000 --> 00:02.000\nm 100 200\n",
			want:    "WEBVTT\n\n00:01.000 --> 00:02.000\nm 100 200\n",
			comment: "same guard: no l/b/s command follows",
		},
		{
			name:    "genuine drawing still goes",
			in:      "WEBVTT\n\n00:01.000 --> 00:02.000\nm 0 0 l 290 0 290 42 0 42\n",
			want:    "WEBVTT",
			comment: "the production cue must keep being dropped",
		},
		{
			name:    "bezier drawing goes too",
			in:      "WEBVTT\n\n00:01.000 --> 00:02.000\nm 0 0 b 1 2 3 4 5 6\n",
			want:    "WEBVTT",
			comment: "curves use b/s rather than l",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(FilterVTTDrawingCues([]byte(tc.in))); got != tc.want {
				t.Errorf("%s\n got: %q\nwant: %q", tc.comment, got, tc.want)
			}
		})
	}
}

func TestSafeFontExt(t *testing.T) {
	// `n` is an untrusted query parameter. Traversal is already impossible
	// (filepath.Ext keeps only the extension), but the extension itself must not
	// be free-form, or a fonts token could litter .unarr/ with arbitrary names.
	tests := map[string]string{
		"arial.ttf":               ".ttf",
		"Adobe Arabic.otf":        ".otf",
		"custom.WOFF2":            ".woff2",
		"evil.php":                ".ttf",
		"a.<svg onload=alert(1)>": ".ttf",
		"../../../../etc/passwd":  ".ttf",
		"noext":                   ".ttf",
		"":                        ".ttf",
	}
	for in, want := range tests {
		if got := SafeFontExt(in); got != want {
			t.Errorf("SafeFontExt(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHasAuthoredStyles(t *testing.T) {
	// `-f ass` on a subrip source does not fail — ffmpeg synthesises a lone
	// "Default" style, producing a script that claims styling nobody wrote.
	synthesised := "[Script Info]\nScriptType: v4.00+\n\n[V4+ Styles]\nFormat: Name, Fontname\nStyle: Default,Arial\n\n[Events]\n"
	if hasAuthoredStyles([]byte(synthesised)) {
		t.Error("ffmpeg's synthesised single Default style must not pass as authored")
	}
	authored := "[Script Info]\nScriptType: v4.00+\n\n[V4+ Styles]\nFormat: Name, Fontname\nStyle: Default,Arial\nStyle: Sign_Basic,Arial\n\n[Events]\n"
	if !hasAuthoredStyles([]byte(authored)) {
		t.Error("a multi-style table is authored")
	}
	named := "[Script Info]\n\n[V4 Styles]\nStyle: Main,Trebuchet MS\n\n[Events]\n"
	if !hasAuthoredStyles([]byte(named)) {
		t.Error("a single NON-Default style is authored (and [V4 Styles] counts)")
	}
	if hasAuthoredStyles([]byte("WEBVTT\n\n00:01.000 --> 00:02.000\nhi\n")) {
		t.Error("a WebVTT body has no style table at all")
	}
}

// Found in PRODUCTION after the 1.11.0 rollout: a sign whose ASS style carries
// Bold=-1 comes out of ffmpeg wrapped in <b>…</b>, so the payload no longer
// STARTS with the move command and the drawing survived into the picture on
// every WebVTT client (Cast, PiP, pre-1.11 agents). Verified against the real
// cue from "The Failed Sage's Academy Domination" S01E02 at 00:43.500.
func TestFilterVTTDrawingCuesStripsInlineTags(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		comment string
	}{
		{
			name:    "bold-wrapped drawing (the production regression)",
			in:      "WEBVTT\n\n00:43.500 --> 00:45.160\n<b>m 0 0 l 268 0 268 88 0 88</b>\n",
			want:    "WEBVTT",
			comment: "the exact cue that survived 1.11.0 in prod",
		},
		{
			name:    "italic-wrapped drawing",
			in:      "WEBVTT\n\n00:01.000 --> 00:02.000\n<i>m 0 0 l 10 0 10 10</i>\n",
			want:    "WEBVTT",
			comment: "italic signs are as common as bold ones",
		},
		{
			name:    "class and voice spans too",
			in:      "WEBVTT\n\n00:01.000 --> 00:02.000\n<c.sign><v Narrator>m 0 0 b 1 2 3 4 5 6</v></c>\n",
			want:    "WEBVTT",
			comment: "WebVTT allows <c.name> and <v Speaker>; both wrap the payload the same way",
		},
		{
			name:    "tagged DIALOGUE is never touched",
			in:      "WEBVTT\n\n00:01.000 --> 00:02.000\n<i>Menos mal que llegamos.</i>\n",
			want:    "WEBVTT\n\n00:01.000 --> 00:02.000\n<i>Menos mal que llegamos.</i>\n",
			comment: "stripping tags must not make ordinary lines look like paths",
		},
		{
			name:    "tagged caption sharing the cue with a path survives whole",
			in:      "WEBVTT\n\n00:01.000 --> 00:02.000\n<b>m 0 0 l 10 0 10 10</b>\n<b>Cartel</b>\n",
			want:    "WEBVTT\n\n00:01.000 --> 00:02.000\n<b>m 0 0 l 10 0 10 10</b>\n<b>Cartel</b>\n",
			comment: "a cue is dropped only when NOTHING in it is renderable text",
		},
		{
			name:    "an empty tag pair is not a drawing",
			in:      "WEBVTT\n\n00:01.000 --> 00:02.000\n<i></i>\n",
			want:    "WEBVTT\n\n00:01.000 --> 00:02.000\n<i></i>\n",
			comment: "stripping leaves \"\", which must not count as a seen drawing line",
		},
		{
			name:    "the served output keeps its markup byte-for-byte",
			in:      "WEBVTT\n\n00:01.000 --> 00:02.000\n<b>m 0 0 l 5 0 5 5</b>\n\n00:03.000 --> 00:04.000\n<i>Diálogo</i>\n",
			want:    "WEBVTT\n\n00:03.000 --> 00:04.000\n<i>Diálogo</i>\n",
			comment: "tags are stripped only to JUDGE; surviving cues are passed through untouched",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(FilterVTTDrawingCues([]byte(tc.in))); got != tc.want {
				t.Errorf("%s\n got: %q\nwant: %q", tc.comment, got, tc.want)
			}
		})
	}
}

// TestFilterVTTDrawingCuesTagSweep exercises the FULL inline-markup vocabulary
// WebVTT allows, in both directions: wrapped drawings must still be dropped, and
// wrapped dialogue must survive byte-for-byte.
func TestFilterVTTDrawingCuesTagSweep(t *testing.T) {
	const path = "m 0 0 l 268 0 268 88 0 88"
	wrappers := map[string]string{
		"bold":            "<b>" + path + "</b>",
		"italic":          "<i>" + path + "</i>",
		"underline":       "<u>" + path + "</u>",
		"class":           "<c.sign.yellow>" + path + "</c>",
		"voice":           "<v Narrator>" + path + "</v>",
		"voice unclosed":  "<v Narrator>" + path,
		"lang":            "<lang ja>" + path + "</lang>",
		"nested":          "<b><i><c.sign>" + path + "</c></i></b>",
		"karaoke stamps":  "<00:00:43.500>" + path + "<00:00:45.160>",
		"mixed nesting":   "<i><b>" + path + "</b></i>",
		"attrs with dots": "<c.a.b.c>" + path + "</c>",
	}
	for name, payload := range wrappers {
		t.Run(name, func(t *testing.T) {
			in := "WEBVTT\n\n00:43.500 --> 00:45.160\n" + payload + "\n"
			got := string(FilterVTTDrawingCues([]byte(in)))
			if strings.Contains(got, "268 88") {
				t.Errorf("drawing survived %s wrapping: %q", name, got)
			}
		})
	}
	// A ruby annotation carries REAL text in its <rt>, so a cue mixing a path
	// with one keeps everything — same rule as a caption sharing the cue.
	t.Run("ruby annotation is renderable text", func(t *testing.T) {
		in := "WEBVTT\n\n00:01.000 --> 00:02.000\n<ruby>" + path + "<rt>x</rt></ruby>\n"
		if got := string(FilterVTTDrawingCues([]byte(in))); got != in {
			t.Errorf("cue with a ruby annotation must survive: %q", got)
		}
	})

	// And the mirror image: real dialogue inside the same wrappers is never lost.
	for name, payload := range map[string]string{
		"bold":   "<b>Hola mundo</b>",
		"voice":  "<v Ryan>Vámonos ya</v>",
		"ruby":   "<ruby>漢<rt>kan</rt></ruby>",
		"stamps": "<00:00:01.000>Canta<00:00:02.000> conmigo",
	} {
		t.Run("keeps "+name, func(t *testing.T) {
			in := "WEBVTT\n\n00:01.000 --> 00:02.000\n" + payload + "\n"
			if got := string(FilterVTTDrawingCues([]byte(in))); got != in {
				t.Errorf("dialogue lost or altered: %q", got)
			}
		})
	}
}

// Angle brackets are prose in real subtitles — the corpus writes skill names as
// "<Resistencia>". Only the WebVTT tag vocabulary may be stripped, or an
// invented tag could leave a fragment the heuristics misread.
func TestStripCueTagsLeavesProseBrackets(t *testing.T) {
	for in, want := range map[string]string{
		"<b>Hola</b>":                "Hola",
		"<Resistencia> mejorada":     "<Resistencia> mejorada",
		"<Manos hábiles>":            "<Manos hábiles>",
		"<c.sign.big>x</c>":          "x",
		"<v Ryan>hola</v>":           "hola",
		"<lang ja>ねこ</lang>":         "ねこ",
		"<00:00:01.500>ya":           "ya",
		"5 < 10 and 10 > 5":          "5 < 10 and 10 > 5",
		"<ruby>漢<rt>kan</rt></ruby>": "漢kan",
	} {
		if got := stripCueTags(in); got != want {
			t.Errorf("stripCueTags(%q) = %q, want %q", in, got, want)
		}
	}
}

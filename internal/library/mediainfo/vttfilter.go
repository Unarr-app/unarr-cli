package mediainfo

import (
	"bytes"
	"regexp"
	"strings"
)

// Fansub .ass tracks carry typeset signs drawn as VECTOR SHAPES: a cue whose
// text is a path (`m 0 0 l 290 0 290 42 0 42`) wrapped in a `{\p1}…{\p0}` block
// that switches libass into drawing mode. The path commands live OUTSIDE the
// braces, so `ffmpeg -c:s webvtt` — which only strips the `{…}` override tags —
// emits the raw coordinates as CUE TEXT. The browser then paints
// "m 0 0 l 290 0 290 42 0 42" across the screen, once per sign, on top of the
// real dialogue. Observed in production and reproduced against the local corpus
// (Exiled Heavy Knight S01E01, 18:37.85 → the "Rampart Reflection" skill card).
//
// A player that renders the .ass natively (VLC, or our own JASSUB canvas) draws
// the sign properly and never sees this. This filter is for everyone else: the
// WebVTT we hand to Chromecast, to older agents' <track> fallback, and to any
// client that only speaks WebVTT. Dropping the cue loses the sign — which the
// client could not have drawn anyway — while keeping the dialogue that shares
// its timestamp readable.
//
// Applied on SERVE rather than on extraction, so it also cleans sidecar .vtt
// files that were cached before this code existed (their mtime is unchanged, so
// re-extracting them would not happen), and so a bad heuristic can be fixed by
// shipping an agent release instead of invalidating every cache on disk.

// drawingPath matches a cue payload that is ENTIRELY an ASS drawing path.
//
// The grammar libass accepts in `\p` mode is a command letter followed by
// coordinate pairs: m/n (move), l (line), b/s (béziers), p/c (spline control,
// close). Both guards must hold for a line to be dropped:
//
//  1. it STARTS with a move command (`m` or `n` + a number) — every well-formed
//     drawing opens with one, and prose effectively never does; and
//  2. it contains NOTHING but command letters, numbers and separators.
//
// The second guard is what protects real dialogue. A line like "m 3 kg de
// arroz" passes guard 1 but dies on guard 2 ("kg", "de", "arroz"), so it stays.
var drawingPath = regexp.MustCompile(`^[mn][ \t]+-?[0-9]`)

// drawingVocabulary is the closed set of characters a pure drawing path can
// contain: the command letters, digits, sign, decimal point and separators.
var drawingVocabulary = regexp.MustCompile(`^[mnlbspc0-9 \t.,+-]+$`)

// isDrawingPayload reports whether every text line of a cue is a drawing path.
// A cue is dropped only when it carries NO renderable text at all — a sign whose
// payload mixes a path with a caption keeps the caption.
func isDrawingPayload(lines []string) bool {
	seen := false
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if !drawingPath.MatchString(t) || !drawingVocabulary.MatchString(t) {
			return false
		}
		seen = true
	}
	return seen
}

// FilterVTTDrawingCues removes cues whose text is an ASS vector-drawing path
// leaked into WebVTT by ffmpeg's ass→webvtt converter (see the file comment).
// Everything else — the WEBVTT header, NOTE/STYLE/REGION blocks, cue settings,
// timings and ordinary cue text — is passed through byte-for-byte.
//
// Input that contains no such cue is returned unchanged, so this is safe to run
// on every subtitle response regardless of the source format.
func FilterVTTDrawingCues(vtt []byte) []byte {
	if len(vtt) == 0 || !bytes.Contains(vtt, []byte("-->")) {
		return vtt
	}

	// Normalise for scanning only; the emitted bytes keep the original line
	// endings of the blocks we pass through.
	crlf := bytes.Contains(vtt, []byte("\r\n"))
	text := string(vtt)
	if crlf {
		text = strings.ReplaceAll(text, "\r\n", "\n")
	}

	blocks := strings.Split(text, "\n\n")
	kept := make([]string, 0, len(blocks))
	dropped := 0

	for _, block := range blocks {
		lines := strings.Split(block, "\n")

		// Find the timing line. Blocks without one (the WEBVTT header, NOTE,
		// STYLE, REGION) are never candidates and pass through untouched.
		timing := -1
		for i, line := range lines {
			if strings.Contains(line, "-->") {
				timing = i
				break
			}
		}
		if timing < 0 {
			kept = append(kept, block)
			continue
		}

		if isDrawingPayload(lines[timing+1:]) {
			dropped++
			continue
		}
		kept = append(kept, block)
	}

	if dropped == 0 {
		return vtt // nothing to do — hand back the original bytes
	}

	out := strings.Join(kept, "\n\n")
	if crlf {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	return []byte(out)
}

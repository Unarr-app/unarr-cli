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

// drawingBody requires at least one command letter AFTER the opening move.
// Every real drawing has one (`l` for lines, `b`/`s` for curves) — the
// production example is `m 0 0 l 290 0 290 42 0 42`. Requiring it stops short
// numeric strings that merely start like a path, e.g. the caption "m 2 3, 4",
// from being mistaken for one: guard 2 alone accepts those, since digits and
// separators are inside the allowed vocabulary.
var drawingBody = regexp.MustCompile(`[ \t][lbspc][ \t]`)

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
		if !drawingPath.MatchString(t) ||
			!drawingVocabulary.MatchString(t) ||
			!drawingBody.MatchString(t) {
			return false
		}
		seen = true
	}
	return seen
}

// filterBlock decides the fate of one VTT block: it returns the text to emit
// and whether a drawing cue was dropped. An empty replacement with drop=true
// means "emit nothing".
func filterBlock(block string) (replacement string, drop bool) {
	lines := strings.Split(block, "\n")

	// Find the timing line. Blocks without one (the WEBVTT header, NOTE, STYLE,
	// REGION) are never candidates and pass through untouched.
	timing := -1
	for i, line := range lines {
		if strings.Contains(line, "-->") {
			timing = i
			break
		}
	}
	if timing < 0 || !isDrawingPayload(lines[timing+1:]) {
		return block, false
	}
	// A malformed file can put the WEBVTT signature in the same block as its
	// first cue (no blank line after the header). Dropping that block would
	// leave a body with no signature at all, which browsers reject outright —
	// strictly worse than leaving one junk cue in place.
	if strings.HasPrefix(block, "WEBVTT") {
		return "WEBVTT", true
	}
	return "", true
}

// FilterVTTDrawingCues removes cues whose text is an ASS vector-drawing path
// leaked into WebVTT by ffmpeg's ass→webvtt converter (see the file comment).
// Everything else — the WEBVTT header, NOTE/STYLE/REGION blocks, cue settings,
// timings and ordinary cue text — is passed through byte-for-byte.
//
// Input that contains no such cue is returned unchanged (same backing array), so
// this is safe to run on every subtitle response regardless of the source
// format. When something IS dropped the output is re-emitted with uniform line
// endings — see the note in the body.
func FilterVTTDrawingCues(vtt []byte) []byte {
	if len(vtt) == 0 || !bytes.Contains(vtt, []byte("-->")) {
		return vtt
	}

	// Line endings are normalised for scanning and RE-EMITTED uniformly: a file
	// containing any CRLF comes back fully CRLF. Mixed-ending input is therefore
	// rewritten, not passed through byte-for-byte. That is harmless for every
	// WebVTT parser and keeps the block logic from having to track endings
	// per-block — but only files that actually need filtering are touched at
	// all, since a clean file returns its original bytes below.
	crlf := bytes.Contains(vtt, []byte("\r\n"))
	text := string(vtt)
	if crlf {
		text = strings.ReplaceAll(text, "\r\n", "\n")
	}

	blocks := strings.Split(text, "\n\n")
	kept := make([]string, 0, len(blocks))
	dropped := 0

	for _, block := range blocks {
		replacement, drop := filterBlock(block)
		if drop {
			dropped++
		}
		if replacement != "" || !drop {
			kept = append(kept, replacement)
		}
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

package mediainfo

import (
	"os"
	"path/filepath"
	"strings"
)

// External (sidecar) subtitle discovery.
//
// A huge share of torrents — anime fansubs especially — ship subtitles as
// SEPARATE files, not embedded streams: a `.srt`/`.ass` named after the video,
// or a bundle inside a `Subs/` (or `Subtitles/`) subfolder. ffprobe on the video
// container never sees these, so the scan recorded zero subtitles for them
// (e.g. ToonsHub "MSubs" releases). This module finds those files so they become
// real, selectable tracks served via the /sub endpoint (path-based, i=-1).
//
// Only TEXT formats are surfaced (srt/ass/ssa/vtt, and a lone .sub). VobSub
// (.idx + .sub) is bitmap — no text form — so it's skipped here; bitmap subs are
// burn-in only and external bitmap burn-in isn't wired.

// subFolderNames are common subfolder names that hold a release's subtitle
// bundle. Matched case-insensitively. Files inside belong to the sibling media.
var subFolderNames = map[string]bool{
	"subs": true, "subtitles": true, "sub": true, "subtitle": true,
}

// sidecarSubExts maps a subtitle file extension to its ffmpeg-style codec name.
// The codec drives the web's text-vs-bitmap classification (isTextSubtitleCodec).
var sidecarSubExts = map[string]string{
	".srt": "subrip",
	".ass": "ass",
	".ssa": "ssa",
	".vtt": "webvtt",
	".sub": "subrip", // MicroDVD/text — UNLESS paired with a .idx (VobSub, handled below)
}

// forcedTokens / sdhTokens are filename markers that refine a sidecar's role.
var forcedTokens = map[string]bool{"forced": true, "forzado": true, "forces": true}
var sdhTokens = map[string]bool{"sdh": true, "cc": true, "hi": false} // "hi" is also Hindi → don't treat as SDH

// sidecarLangAliases maps RELEASE-NAMING subtitle tokens (fansub/scene shorthand
// NOT covered by the ISO 639-1/2 normaliser) to a language hint. Two things make
// this necessary beyond NormalizeLang:
//   - Chinese SCRIPT matters for charset: Simplified (chs/sc/gb) is GBK,
//     Traditional (cht/tc/big5) is Big5 — decoding one as the other is mojibake.
//     We keep the script in the hint ("zh" vs "zh-Hant") so legacyEncodingForLang
//     picks the right code page. Anime fansubs routinely ship both.
//   - lat/latino/vostfr etc. aren't ISO at all and would fall to "und".
//
// Applied ONLY to sidecar filenames, not ffprobe metadata, so it can't clash with
// the global langNormalize ("lat"→Latin there). Plain ISO codes (eng/spa/…) are
// intentionally left to NormalizeLang.
var sidecarLangAliases = map[string]string{
	"chs": "zh", "sc": "zh", "gb": "zh", "gbk": "zh", "hans": "zh", // Simplified → GBK
	"cht": "zh-Hant", "tc": "zh-Hant", "big5": "zh-Hant", "hant": "zh-Hant", // Traditional → Big5
	"lat": "es", "latino": "es", "esp": "es", "español": "es", "espanol": "es",
	"vostfr": "fr", "vff": "fr", "vf": "fr",
	"ptbr": "pt", "pt-br": "pt", "bra": "pt",
}

// DiscoverSidecarSubtitles finds external subtitle files for a local media file:
// siblings named after the video, plus everything in a Subs/Subtitles subfolder.
// Returns text tracks only, each with External=true and an absolute Path. Safe on
// any path — returns nil if the directory can't be read (best-effort, like the
// rest of the scan). Never call for a remote URL source (no local directory).
//
// NOTE: discovered sidecars are NOT deduped against embedded streams of the same
// language. That's deliberate — a `Movie.en.srt` next to a video that also has an
// embedded English stream is usually a DIFFERENT track (full vs SDH, retimed, or
// a better translation), so silently dropping either would hide a choice the user
// may want. Both surface as separate, distinctly-labelled entries.
func DiscoverSidecarSubtitles(mediaPath string) []SubtitleTrack {
	if mediaPath == "" || strings.Contains(mediaPath, "://") {
		return nil
	}
	dir := filepath.Dir(mediaPath)
	videoBase := strings.TrimSuffix(filepath.Base(mediaPath), filepath.Ext(mediaPath))
	videoBaseLower := strings.ToLower(videoBase)

	var out []SubtitleTrack
	seen := make(map[string]bool) // absolute path dedupe

	// 1. Siblings in the media's own directory whose name starts with the video
	//    base name: "Movie.srt", "Movie.en.srt", "Movie.en.forced.ass", …
	addFromDir(dir, func(name string) bool {
		return strings.HasPrefix(strings.ToLower(name), videoBaseLower)
	}, videoBase, &out, seen)

	// 2. A Subs/Subtitles subfolder: take EVERY subtitle file (the whole folder
	//    belongs to this release). Filenames there are usually language-named
	//    ("2_English.srt", "spa.ass") with no video-base prefix.
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if e.IsDir() && subFolderNames[strings.ToLower(e.Name())] {
				addFromDir(filepath.Join(dir, e.Name()), func(string) bool { return true }, "", &out, seen)
			}
		}
	}
	return out
}

// addFromDir scans one directory, emitting a SubtitleTrack for each text sidecar
// whose name passes `match`. stripPrefix (the video base, may be "") is removed
// before parsing language/role tokens so "Movie.en.forced.srt" parses as "en"+forced.
func addFromDir(dir string, match func(name string) bool, stripPrefix string, out *[]SubtitleTrack, seen map[string]bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	// Pre-index .idx files so a paired .sub is recognised as VobSub (bitmap) and skipped.
	idxBases := make(map[string]bool)
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".idx") {
			idxBases[strings.ToLower(strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())))] = true
		}
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		codec, ok := sidecarSubExts[ext]
		if !ok || !match(name) {
			continue
		}
		// VobSub: a .sub paired with a same-named .idx is bitmap, not text. Skip.
		if ext == ".sub" && idxBases[strings.ToLower(strings.TrimSuffix(name, ext))] {
			continue
		}
		abs := filepath.Join(dir, name)
		if seen[abs] {
			continue
		}
		seen[abs] = true

		lang, forced, title := parseSidecarName(name, ext, stripPrefix)
		*out = append(*out, SubtitleTrack{
			Lang:     lang,
			Codec:    codec,
			Title:    title,
			Forced:   forced,
			External: true,
			Path:     abs,
		})
	}
}

// parseSidecarName extracts (lang, forced, title) from a subtitle filename.
// stripPrefix (the video base) is removed first; the remainder is tokenised on
// common separators and scanned for a language code + role markers. Unknown →
// lang "und". The title is a human hint ("Forced", "SDH") or "".
func parseSidecarName(name, ext, stripPrefix string) (lang string, forced bool, title string) {
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	if stripPrefix != "" && len(stem) >= len(stripPrefix) &&
		strings.EqualFold(stem[:len(stripPrefix)], stripPrefix) {
		stem = stem[len(stripPrefix):]
	}
	lang = "und"
	var roles []string
	for _, tok := range strings.FieldsFunc(stem, func(r rune) bool {
		return r == '.' || r == '_' || r == '-' || r == ' ' || r == '[' || r == ']' || r == '(' || r == ')'
	}) {
		low := strings.ToLower(strings.TrimSpace(tok))
		if low == "" {
			continue
		}
		if forcedTokens[low] {
			forced = true
			roles = append(roles, "Forced")
			continue
		}
		if v, isSDH := sdhTokens[low]; isSDH && v {
			roles = append(roles, "SDH")
			continue
		}
		// First token that maps to a real language wins. Try release-naming
		// aliases (chs/lat/…) first, then the standard ISO normaliser. NormalizeLang
		// echoes unknown input back lowercased, so accept only a mapped result
		// (different from the raw token, or already a known 2-letter code).
		if lang == "und" {
			if alias, ok := sidecarLangAliases[low]; ok {
				lang = alias
				continue
			}
			if norm := NormalizeLang(low); norm != "und" && (norm != low || len(low) == 2) && isKnownLang(norm) {
				lang = norm
				continue
			}
		}
	}
	title = strings.Join(roles, " ")
	return lang, forced, title
}

// isKnownLang reports whether code is a value present in langNormalize (i.e. a
// real ISO 639-1 we recognise) — guards against treating a random filename token
// ("web", "dl") as a language.
func isKnownLang(code string) bool {
	for _, v := range langNormalize {
		if v == code {
			return true
		}
	}
	return false
}

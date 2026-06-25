package mediainfo

import (
	"bytes"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// Subtitle charset normalisation.
//
// External subtitle files are routinely NOT UTF-8: legacy .srt files come in the
// uploader's local code page (Windows-1252 Western, Windows-1256 Arabic, GBK
// Chinese, Shift-JIS Japanese, …). Feeding those raw to ffmpeg → WebVTT yields
// mojibake. We detect the encoding and transcode to UTF-8 before extraction.
//
// Detection order: BOM (authoritative) → valid UTF-8 → a code page chosen from
// the track's declared language (from its filename, e.g. ".ar.srt"). The
// language hint is the reliable signal we have without a full statistical
// detector: an Arabic sub that isn't UTF-8 is almost certainly Windows-1256, a
// Russian one Windows-1251, and so on. Western European is the safe default.

// legacyEncodingForLang returns the most likely single-byte / CJK encoding for a
// non-UTF-8 subtitle in the given language hint. The hint is normally an ISO
// 639-1 code, but Chinese carries a script suffix ("zh-hant" / "zh-tw") so a
// Traditional sidecar decodes as Big5 instead of GBK (decoding Big5 bytes as GBK
// is mojibake — and anime fansubs routinely ship both chs AND cht). Default:
// Windows-1252.
func legacyEncodingForLang(lang string) encoding.Encoding {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "ar", "fa", "ur": // Arabic script
		return charmap.Windows1256
	case "ru", "uk", "bg", "sr", "mk": // Cyrillic
		return charmap.Windows1251
	case "el": // Greek
		return charmap.Windows1253
	case "he": // Hebrew
		return charmap.Windows1255
	case "tr": // Turkish
		return charmap.Windows1254
	case "th": // Thai
		return charmap.Windows874
	case "zh-hant", "zh_hant", "zh-tw", "zh-hk", "zhtw": // Traditional Chinese
		return traditionalchinese.Big5
	case "zh", "zh-hans", "zh-cn": // Simplified Chinese (covers most pirate releases)
		return simplifiedchinese.GBK
	case "ja": // Japanese
		return japanese.ShiftJIS
	case "ko": // Korean
		return korean.EUCKR
	case "vi": // Vietnamese
		return charmap.Windows1258
	case "pl", "cs", "sk", "hu", "ro", "hr", "sl": // Central European
		return charmap.Windows1250
	case "lt", "lv", "et": // Baltic
		return charmap.Windows1257
	default: // Western European + everything else
		return charmap.Windows1252
	}
}

// DecodeSubtitleToUTF8 returns the bytes as UTF-8, transcoding from a detected
// legacy encoding when needed. The returned name is for logging ("utf-8",
// "bom-utf16le", "windows-1256", …). Never fails: a transcode error falls back
// to the original bytes (ffmpeg may still cope).
func DecodeSubtitleToUTF8(data []byte, langHint string) ([]byte, string) {
	// BOM wins — it's unambiguous.
	switch {
	case bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}):
		//nolint:gosec // G602: HasPrefix matched a 3-byte BOM, so len(data) >= 3 — the slice bound is provably in range.
		return data[3:], "bom-utf8"
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE}):
		return decodeWith(data, unicode.UTF16(unicode.LittleEndian, unicode.UseBOM), "bom-utf16le")
	case bytes.HasPrefix(data, []byte{0xFE, 0xFF}):
		return decodeWith(data, unicode.UTF16(unicode.BigEndian, unicode.UseBOM), "bom-utf16be")
	}
	// Already valid UTF-8 → no transcode (ASCII is a subset, so plain English
	// srt files hit this).
	if utf8.Valid(data) {
		return data, "utf-8"
	}
	// Non-UTF-8: transcode from the language's likely code page.
	enc := legacyEncodingForLang(langHint)
	out, name := decodeWith(data, enc, encodingName(enc))
	return out, name
}

// decodeWith transforms data through enc's decoder to UTF-8. On error returns the
// original bytes (best-effort) with the name suffixed "(raw)".
func decodeWith(data []byte, enc encoding.Encoding, name string) ([]byte, string) {
	out, _, err := transform.Bytes(enc.NewDecoder(), data)
	if err != nil || len(out) == 0 {
		return data, name + "(raw)"
	}
	return out, name
}

// encodingName maps a known encoding back to a short label for logs.
func encodingName(enc encoding.Encoding) string {
	switch enc {
	case charmap.Windows1250:
		return "windows-1250"
	case charmap.Windows1251:
		return "windows-1251"
	case charmap.Windows1252:
		return "windows-1252"
	case charmap.Windows1253:
		return "windows-1253"
	case charmap.Windows1254:
		return "windows-1254"
	case charmap.Windows1255:
		return "windows-1255"
	case charmap.Windows1256:
		return "windows-1256"
	case charmap.Windows1257:
		return "windows-1257"
	case charmap.Windows1258:
		return "windows-1258"
	case charmap.Windows874:
		return "windows-874"
	case simplifiedchinese.GBK:
		return "gbk"
	case traditionalchinese.Big5:
		return "big5"
	case japanese.ShiftJIS:
		return "shift-jis"
	case korean.EUCKR:
		return "euc-kr"
	default:
		return "legacy"
	}
}

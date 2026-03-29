package mediainfo

import (
	"sort"
	"strings"
)

// langNormalize maps ISO 639-2/B, 639-2/T, 639-1 codes, and full English
// language names (as returned by some ffprobe metadata) to ISO 639-1.
var langNormalize = map[string]string{
	// ISO codes
	"eng": "en", "en": "en",
	"spa": "es", "es": "es",
	"fre": "fr", "fra": "fr", "fr": "fr",
	"ger": "de", "deu": "de", "de": "de",
	"ita": "it", "it": "it",
	"por": "pt", "pt": "pt",
	"rus": "ru", "ru": "ru",
	"jpn": "ja", "ja": "ja",
	"kor": "ko", "ko": "ko",
	"chi": "zh", "zho": "zh", "zh": "zh",
	"hin": "hi", "hi": "hi",
	"ara": "ar", "ar": "ar",
	"dut": "nl", "nld": "nl", "nl": "nl",
	"pol": "pl", "pl": "pl",
	"tur": "tr", "tr": "tr",
	"swe": "sv", "sv": "sv",
	"nor": "no", "nob": "no", "nno": "no", "no": "no",
	"dan": "da", "da": "da",
	"fin": "fi", "fi": "fi",
	"cze": "cs", "ces": "cs", "cs": "cs",
	"hun": "hu", "hu": "hu",
	"rum": "ro", "ron": "ro", "ro": "ro",
	"gre": "el", "ell": "el", "el": "el",
	"tha": "th", "th": "th",
	"vie": "vi", "vi": "vi",
	"ind": "id", "id": "id",
	"heb": "he", "he": "he",
	"ukr": "uk", "uk": "uk",
	"cat": "ca", "ca": "ca",
	"bul": "bg", "bg": "bg",
	"hrv": "hr", "hr": "hr",
	"srp": "sr", "sr": "sr",
	"slv": "sl", "sl": "sl",
	"lit": "lt", "lt": "lt",
	"lav": "lv", "lv": "lv",
	"est": "et", "et": "et",
	"per": "fa", "fas": "fa", "fa": "fa",
	"may": "ms", "msa": "ms", "ms": "ms",
	"tgl": "tl", "tl": "tl",
	"tam": "ta", "ta": "ta",
	"tel": "te", "te": "te",
	"ben": "bn", "bn": "bn",
	"urd": "ur", "ur": "ur",
	"geo": "ka", "kat": "ka", "ka": "ka",
	"arm": "hy", "hye": "hy", "hy": "hy",
	"alb": "sq", "sqi": "sq", "sq": "sq",
	"mac": "mk", "mkd": "mk", "mk": "mk",
	"ice": "is", "isl": "is", "is": "is",
	"glg": "gl", "gl": "gl",
	"baq": "eu", "eus": "eu", "eu": "eu",
	"wel": "cy", "cym": "cy", "cy": "cy",
	"gle": "ga", "ga": "ga",
	"mlt": "mt", "mt": "mt",
	"swa": "sw", "sw": "sw",
	"afr": "af", "af": "af",
	"lat": "la", "la": "la",

	// Full English names (ffprobe sometimes returns these instead of codes)
	"english": "en", "spanish": "es", "french": "fr", "german": "de",
	"italian": "it", "portuguese": "pt", "russian": "ru", "japanese": "ja",
	"korean": "ko", "chinese": "zh", "hindi": "hi", "arabic": "ar",
	"dutch": "nl", "polish": "pl", "turkish": "tr", "swedish": "sv",
	"norwegian": "no", "danish": "da", "finnish": "fi", "czech": "cs",
	"hungarian": "hu", "romanian": "ro", "greek": "el", "thai": "th",
	"vietnamese": "vi", "indonesian": "id", "hebrew": "he", "ukrainian": "uk",
	"catalan": "ca", "bulgarian": "bg", "croatian": "hr", "serbian": "sr",
	"slovenian": "sl", "lithuanian": "lt", "latvian": "lv", "estonian": "et",
	"persian": "fa", "malay": "ms", "tagalog": "tl", "tamil": "ta",
	"telugu": "te", "bengali": "bn", "urdu": "ur", "georgian": "ka",
	"armenian": "hy", "albanian": "sq", "macedonian": "mk", "icelandic": "is",
	"galician": "gl", "basque": "eu", "welsh": "cy", "irish": "ga",
	"maltese": "mt", "swahili": "sw", "afrikaans": "af", "latin": "la",
}

// NormalizeLang converts a language code to ISO 639-1.
// Returns "und" for empty input, the input lowercased if no mapping is found.
func NormalizeLang(raw string) string {
	if raw == "" {
		return "und"
	}
	lower := strings.ToLower(raw)
	if mapped, ok := langNormalize[lower]; ok {
		return mapped
	}
	return lower
}

// ComputeLanguages extracts unique ISO 639-1 language codes from audio tracks.
func ComputeLanguages(audioTracks []AudioTrack) []string {
	seen := make(map[string]struct{})
	for _, t := range audioTracks {
		lang := t.Lang
		if lang != "" && lang != "und" && len(lang) <= 3 {
			seen[lang] = struct{}{}
		}
	}

	result := make([]string, 0, len(seen))
	for l := range seen {
		result = append(result, l)
	}
	sort.Strings(result)
	return result
}

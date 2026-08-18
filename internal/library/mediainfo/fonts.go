package mediainfo

import (
	"path/filepath"
	"strings"
)

// fontMimetypes are the MIME types muxers write for embedded font attachments.
// The list is deliberately broad: MKVToolNix, ffmpeg and hand-rolled fansub
// scripts all disagree on the "correct" type for the same file.
var fontMimetypes = map[string]bool{
	"application/x-truetype-font": true,
	"application/vnd.ms-opentype": true,
	"application/x-font-ttf":      true,
	"application/x-font-otf":      true,
	"application/font-sfnt":       true,
	"application/font-woff":       true,
	"font/ttf":                    true,
	"font/otf":                    true,
	"font/sfnt":                   true,
	"font/woff":                   true,
	"font/woff2":                  true,
	"application/x-font-truetype": true,
	"application/x-font-opentype": true,
}

// fontExtensions is the second opinion, needed because the mimetype LIES. Real
// releases ship OpenType files declared as `application/x-truetype-font`
// (observed on every AdobeArabic-*.otf in Skeleton Knight S01E01), and some
// muxers fall back to `application/octet-stream` for everything. Trusting the
// mimetype alone would drop fonts an .ass track genuinely needs.
var fontExtensions = map[string]bool{
	".ttf":   true,
	".otf":   true,
	".ttc":   true,
	".otc":   true,
	".woff":  true,
	".woff2": true,
	".pfb":   true,
}

// isFontAttachment reports whether a container attachment is a font, and so
// worth extracting for an .ass renderer. Either signal is enough: a recognised
// mimetype, or a recognised filename extension.
//
// Non-font attachments (cover art, chapter XML, the odd README) are excluded —
// but note they still advance FontAttachment.Index, since ffmpeg's
// -dump_attachment:t:N counts every attachment stream.
func isFontAttachment(filename, mimetype string) bool {
	if fontMimetypes[strings.ToLower(strings.TrimSpace(mimetype))] {
		return true
	}
	return fontExtensions[strings.ToLower(filepath.Ext(filename))]
}

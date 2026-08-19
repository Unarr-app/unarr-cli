package mediainfo

import (
	"encoding/json"
	"testing"
)

func TestIsFontAttachment(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		mimetype string
		want     bool
	}{
		// Real rows from Skeleton Knight S01E01 / Exiled Heavy Knight S01E01.
		{"truetype declared honestly", "arialbd.ttf", "application/x-truetype-font", true},
		{"opentype declared honestly", "CASINO HAND.OTF", "application/vnd.ms-opentype", true},
		{"OTF MISDECLARED as truetype", "AdobeArabic-Regular.otf", "application/x-truetype-font", true},
		{"filename with spaces", "MHGHagoromo T HK Medium.ttf", "application/x-truetype-font", true},
		{"uppercase extension", "ariblk_0.TTF", "", true},

		// Extension-only rescue: some muxers write octet-stream for everything.
		{"octet-stream but .ttf", "trebuc.ttf", "application/octet-stream", true},
		{"octet-stream but .woff2", "custom.woff2", "application/octet-stream", true},

		// Genuine non-fonts must be skipped.
		{"cover art", "cover.jpg", "image/jpeg", false},
		{"chapter xml", "chapters.xml", "application/xml", false},
		{"readme", "README.txt", "text/plain", false},
		{"no filename, no mimetype", "", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isFontAttachment(tc.filename, tc.mimetype); got != tc.want {
				t.Errorf("isFontAttachment(%q, %q) = %v, want %v", tc.filename, tc.mimetype, got, tc.want)
			}
		})
	}
}

// TestParseFFprobeAttachmentIndexing pins the invariant that breaks silently:
// FontAttachment.Index must be the position among ATTACHMENT streams (ffmpeg's
// `t:N`), not the global stream index and not the position after filtering to
// fonts. Getting it wrong dumps the wrong file with no error.
func TestParseFFprobeAttachmentIndexing(t *testing.T) {
	// Shaped after a real fansub MKV: video + audio + subtitle occupy the low
	// stream indices, then attachments — one of which is NOT a font.
	raw := `{"streams":[
		{"index":0,"codec_type":"video","codec_name":"h264","width":1920,"height":1080},
		{"index":1,"codec_type":"audio","codec_name":"aac","channels":2,"tags":{"language":"jpn"}},
		{"index":2,"codec_type":"subtitle","codec_name":"ass","tags":{"language":"eng"}},
		{"index":16,"codec_type":"attachment","tags":{"filename":"arial.ttf","mimetype":"application/x-truetype-font"}},
		{"index":17,"codec_type":"attachment","tags":{"filename":"cover.jpg","mimetype":"image/jpeg"}},
		{"index":18,"codec_type":"attachment","tags":{"filename":"AdobeArabic-Regular.otf","mimetype":"application/x-truetype-font"}}
	],"format":{}}`

	var data ffprobeOutput
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatalf("fixture does not parse: %v", err)
	}
	info, err := parseFFprobeOutput(data)
	if err != nil {
		t.Fatalf("parseFFprobeOutput: %v", err)
	}

	if len(info.Fonts) != 2 {
		t.Fatalf("got %d fonts, want 2 (cover.jpg must be skipped): %+v", len(info.Fonts), info.Fonts)
	}
	if info.Fonts[0].Index != 0 || info.Fonts[0].Filename != "arial.ttf" {
		t.Errorf("first font = %+v, want index 0 / arial.ttf", info.Fonts[0])
	}
	// The .otf is the THIRD attachment (t:2) even though it is the SECOND font,
	// and its global stream index is 18. Only t:2 dumps the right bytes.
	if info.Fonts[1].Index != 2 {
		t.Errorf("second font Index = %d, want 2 (attachment position, not font position (1) nor stream index (18))", info.Fonts[1].Index)
	}
}

func TestParseFFprobeNoAttachments(t *testing.T) {
	raw := `{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","width":1920,"height":1080}],"format":{}}`
	var data ffprobeOutput
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatalf("fixture does not parse: %v", err)
	}
	info, err := parseFFprobeOutput(data)
	if err != nil {
		t.Fatalf("parseFFprobeOutput: %v", err)
	}
	if len(info.Fonts) != 0 {
		t.Errorf("got %d fonts for a file with none, want 0", len(info.Fonts))
	}
}

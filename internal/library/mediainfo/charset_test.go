package mediainfo

import (
	"testing"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/transform"
)

func TestDecodeSubtitleToUTF8_PlainASCII(t *testing.T) {
	in := []byte("Hello world")
	out, name := DecodeSubtitleToUTF8(in, "en")
	if string(out) != "Hello world" || name != "utf-8" {
		t.Fatalf("ASCII passthrough failed: %q %s", out, name)
	}
}

func TestDecodeSubtitleToUTF8_BOMStripped(t *testing.T) {
	in := append([]byte{0xEF, 0xBB, 0xBF}, []byte("café")...)
	out, name := DecodeSubtitleToUTF8(in, "fr")
	if string(out) != "café" || name != "bom-utf8" {
		t.Fatalf("UTF-8 BOM strip failed: %q %s", out, name)
	}
}

func TestDecodeSubtitleToUTF8_Windows1252(t *testing.T) {
	// "café" encoded in Windows-1252 (é = 0xE9) is NOT valid UTF-8.
	enc1252, _, err := transform.Bytes(charmap.Windows1252.NewEncoder(), []byte("café"))
	if err != nil {
		t.Fatal(err)
	}
	out, name := DecodeSubtitleToUTF8(enc1252, "fr")
	if string(out) != "café" {
		t.Fatalf("Windows-1252 decode failed: got %q (%s)", out, name)
	}
	if name != "windows-1252" {
		t.Fatalf("expected windows-1252, got %s", name)
	}
}

func TestDecodeSubtitleToUTF8_TraditionalChineseBig5(t *testing.T) {
	// 繁 (U+7E41) in Big5 is 0xC1 0x63. Decoding it as GBK would be mojibake, so
	// the zh-Hant hint must route to Big5.
	in := []byte{0xC1, 0x63}
	out, name := DecodeSubtitleToUTF8(in, "zh-Hant")
	if name != "big5" {
		t.Fatalf("expected big5 for zh-Hant, got %s", name)
	}
	if string(out) != "繁" {
		t.Fatalf("Big5 decode failed: got %q", out)
	}
}

func TestDecodeSubtitleToUTF8_ArabicByLang(t *testing.T) {
	// Arabic letter ا (U+0627) is 0xC7 in Windows-1256.
	in := []byte{0xC7}
	out, name := DecodeSubtitleToUTF8(in, "ar")
	if name != "windows-1256" {
		t.Fatalf("expected windows-1256 for Arabic, got %s", name)
	}
	if string(out) != "ا" {
		t.Fatalf("Arabic decode failed: got %q", out)
	}
}

package mediainfo

import "testing"

func TestNormalizeLang(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", "und"},
		{"eng", "en"},
		{"spa", "es"},
		{"fre", "fr"},
		{"fra", "fr"},
		{"ger", "de"},
		{"deu", "de"},
		{"en", "en"},
		{"es", "es"},
		{"English", "en"},
		{"SPANISH", "es"},
		{"Japanese", "ja"},
		{"jpn", "ja"},
		{"chi", "zh"},
		{"zho", "zh"},
		{"und", "und"},
		{"xyz", "xyz"}, // unknown → lowercase passthrough
		{"POR", "pt"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := NormalizeLang(tt.input)
			if got != tt.want {
				t.Errorf("NormalizeLang(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestComputeLanguages(t *testing.T) {
	tracks := []AudioTrack{
		{Lang: "en", Codec: "aac", Channels: 2},
		{Lang: "es", Codec: "ac3", Channels: 6},
		{Lang: "en", Codec: "dts", Channels: 6}, // duplicate
		{Lang: "und", Codec: "aac", Channels: 2},
		{Lang: "", Codec: "aac", Channels: 2},
	}

	langs := ComputeLanguages(tracks)

	if len(langs) != 2 {
		t.Fatalf("expected 2 languages, got %d: %v", len(langs), langs)
	}
	if langs[0] != "en" || langs[1] != "es" {
		t.Errorf("expected [en es], got %v", langs)
	}
}

func TestComputeLanguagesEmpty(t *testing.T) {
	langs := ComputeLanguages(nil)
	if len(langs) != 0 {
		t.Errorf("expected empty, got %v", langs)
	}
}

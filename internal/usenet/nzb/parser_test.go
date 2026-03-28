package nzb

import (
	"strings"
	"testing"
)

const testNZB = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE nzb PUBLIC "-//newzBin//DTD NZB 1.1//EN" "http://www.newzbin.com/DTD/nzb/nzb-1.1.dtd">
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file poster="user@example.com" date="1700000000" subject="Movie.2024.1080p.BluRay.x264-GROUP [01/50] - &quot;Movie.2024.1080p.BluRay.x264-GROUP.mkv&quot; yEnc (1/3200)">
    <groups>
      <group>alt.binaries.movies</group>
      <group>alt.binaries.multimedia</group>
    </groups>
    <segments>
      <segment bytes="768000" number="1">abc123@news.example.com</segment>
      <segment bytes="768000" number="2">def456@news.example.com</segment>
      <segment bytes="512000" number="3">ghi789@news.example.com</segment>
    </segments>
  </file>
  <file poster="user@example.com" date="1700000000" subject="Movie.2024.1080p.BluRay.x264-GROUP [02/50] - &quot;Movie.2024.1080p.BluRay.x264-GROUP.nfo&quot; yEnc (1/1)">
    <groups>
      <group>alt.binaries.movies</group>
    </groups>
    <segments>
      <segment bytes="4096" number="1">nfo001@news.example.com</segment>
    </segments>
  </file>
  <file poster="user@example.com" date="1700000000" subject="Movie.2024.1080p.BluRay.x264-GROUP [03/50] - &quot;Movie.2024.1080p.BluRay.x264-GROUP.par2&quot; yEnc (1/1)">
    <groups>
      <group>alt.binaries.movies</group>
    </groups>
    <segments>
      <segment bytes="32768" number="1">par001@news.example.com</segment>
    </segments>
  </file>
</nzb>`

const testNZBWithRars = `<?xml version="1.0" encoding="UTF-8"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file poster="bot@example.com" date="1700000000" subject="[PRiVATE]-[#a]- &quot;Movie.2024.rar&quot; yEnc (01/99)">
    <groups><group>alt.binaries.movies</group></groups>
    <segments>
      <segment bytes="768000" number="1">rar001@example</segment>
      <segment bytes="768000" number="2">rar002@example</segment>
    </segments>
  </file>
  <file poster="bot@example.com" date="1700000000" subject="[PRiVATE]-[#a]- &quot;Movie.2024.r00&quot; yEnc (01/99)">
    <groups><group>alt.binaries.movies</group></groups>
    <segments>
      <segment bytes="768000" number="1">r00001@example</segment>
    </segments>
  </file>
  <file poster="bot@example.com" date="1700000000" subject="[PRiVATE]-[#a]- &quot;Movie.2024.r01&quot; yEnc (01/99)">
    <groups><group>alt.binaries.movies</group></groups>
    <segments>
      <segment bytes="768000" number="1">r01001@example</segment>
    </segments>
  </file>
  <file poster="bot@example.com" date="1700000000" subject="[PRiVATE]-[#a]- &quot;Movie.2024.par2&quot; yEnc (1/1)">
    <groups><group>alt.binaries.movies</group></groups>
    <segments>
      <segment bytes="32768" number="1">par001@example</segment>
    </segments>
  </file>
</nzb>`

func TestParse(t *testing.T) {
	nzb, err := Parse(strings.NewReader(testNZB))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if len(nzb.Files) != 3 {
		t.Fatalf("expected 3 files, got %d", len(nzb.Files))
	}

	// First file — the MKV
	f := nzb.Files[0]
	if f.Poster != "user@example.com" {
		t.Errorf("poster: got %q", f.Poster)
	}
	if f.Date != 1700000000 {
		t.Errorf("date: got %d", f.Date)
	}
	if len(f.Groups) != 2 {
		t.Errorf("groups: got %d", len(f.Groups))
	}
	if f.Groups[0] != "alt.binaries.movies" {
		t.Errorf("group[0]: got %q", f.Groups[0])
	}
	if len(f.Segments) != 3 {
		t.Errorf("segments: got %d", len(f.Segments))
	}

	seg := f.Segments[0]
	if seg.Bytes != 768000 {
		t.Errorf("seg bytes: got %d", seg.Bytes)
	}
	if seg.Number != 1 {
		t.Errorf("seg number: got %d", seg.Number)
	}
	if seg.MessageID != "abc123@news.example.com" {
		t.Errorf("seg msgid: got %q", seg.MessageID)
	}
}

func TestParseBytes(t *testing.T) {
	nzb, err := ParseBytes([]byte(testNZB))
	if err != nil {
		t.Fatalf("ParseBytes failed: %v", err)
	}
	if len(nzb.Files) != 3 {
		t.Fatalf("expected 3 files, got %d", len(nzb.Files))
	}
}

func TestTotalBytes(t *testing.T) {
	nzb, _ := ParseBytes([]byte(testNZB))
	// 768000 + 768000 + 512000 + 4096 + 32768
	expected := int64(768000 + 768000 + 512000 + 4096 + 32768)
	if got := nzb.TotalBytes(); got != expected {
		t.Errorf("TotalBytes: got %d, want %d", got, expected)
	}
}

func TestTotalSegments(t *testing.T) {
	nzb, _ := ParseBytes([]byte(testNZB))
	if got := nzb.TotalSegments(); got != 5 {
		t.Errorf("TotalSegments: got %d, want 5", got)
	}
}

func TestContentFiles(t *testing.T) {
	nzb, _ := ParseBytes([]byte(testNZB))
	content := nzb.ContentFiles()
	if len(content) != 1 {
		t.Fatalf("ContentFiles: got %d, want 1", len(content))
	}
	if content[0].Filename() != "Movie.2024.1080p.BluRay.x264-GROUP.mkv" {
		t.Errorf("content filename: got %q", content[0].Filename())
	}
}

func TestPar2Files(t *testing.T) {
	nzb, _ := ParseBytes([]byte(testNZB))
	par2 := nzb.Par2Files()
	if len(par2) != 1 {
		t.Fatalf("Par2Files: got %d, want 1", len(par2))
	}
}

func TestLargestFile(t *testing.T) {
	nzb, _ := ParseBytes([]byte(testNZB))
	largest := nzb.LargestFile()
	if largest == nil {
		t.Fatal("LargestFile returned nil")
	}
	if largest.Filename() != "Movie.2024.1080p.BluRay.x264-GROUP.mkv" {
		t.Errorf("largest file: got %q", largest.Filename())
	}
}

func TestFilename(t *testing.T) {
	tests := []struct {
		subject  string
		expected string
	}{
		{
			`Movie.2024.1080p [01/50] - "Movie.2024.1080p.mkv" yEnc (1/3200)`,
			"Movie.2024.1080p.mkv",
		},
		{
			`[PRiVATE]-[#a]- "file.rar" yEnc (01/99)`,
			"file.rar",
		},
		{
			`Some subject without quotes (1/1)`,
			"Some subject without quotes",
		},
	}

	for _, tt := range tests {
		f := File{Subject: tt.subject}
		if got := f.Filename(); got != tt.expected {
			t.Errorf("Filename(%q) = %q, want %q", tt.subject, got, tt.expected)
		}
	}
}

func TestExtension(t *testing.T) {
	f := File{Subject: `"Movie.2024.1080p.BluRay.x264-GROUP.mkv" yEnc (1/3200)`}
	if got := f.Extension(); got != ".mkv" {
		t.Errorf("Extension: got %q, want .mkv", got)
	}
}

func TestHasRars(t *testing.T) {
	nzb, _ := ParseBytes([]byte(testNZBWithRars))
	if !nzb.HasRars() {
		t.Error("HasRars: expected true")
	}
	if !nzb.HasPar2() {
		t.Error("HasPar2: expected true")
	}
}

func TestRarFiles(t *testing.T) {
	nzb, _ := ParseBytes([]byte(testNZBWithRars))
	rars := nzb.RarFiles()
	if len(rars) != 3 {
		t.Fatalf("RarFiles: got %d, want 3", len(rars))
	}
}

func TestIsRarFile(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"file.rar", true},
		{"file.r00", true},
		{"file.r99", true},
		{"file.s00", true},
		{"file.001", true},
		{"file.mkv", false},
		{"file.par2", false},
		{"file.nfo", false},
	}
	for _, tt := range tests {
		if got := isRarFile(tt.name); got != tt.want {
			t.Errorf("isRarFile(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestParseEmpty(t *testing.T) {
	_, err := Parse(strings.NewReader(`<?xml version="1.0"?><nzb xmlns="http://www.newzbin.com/DTD/2003/nzb"></nzb>`))
	if err == nil {
		t.Error("expected error for empty NZB")
	}
}

func TestParseInvalidXML(t *testing.T) {
	_, err := Parse(strings.NewReader("not xml"))
	if err == nil {
		t.Error("expected error for invalid XML")
	}
}

func TestStripAngleBrackets(t *testing.T) {
	nzbXML := `<?xml version="1.0"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file poster="test" date="0" subject="&quot;test.bin&quot; (1/1)">
    <groups><group>alt.test</group></groups>
    <segments>
      <segment bytes="100" number="1">&lt;angle@brackets.com&gt;</segment>
    </segments>
  </file>
</nzb>`
	nzb, err := ParseBytes([]byte(nzbXML))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if nzb.Files[0].Segments[0].MessageID != "angle@brackets.com" {
		t.Errorf("MessageID not stripped: got %q", nzb.Files[0].Segments[0].MessageID)
	}
}

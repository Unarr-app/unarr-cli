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

// --- Malformed / edge-case XML inputs ---

func TestParse_CompletelyEmpty(t *testing.T) {
	_, err := Parse(strings.NewReader(""))
	if err == nil {
		t.Error("expected error for completely empty input")
	}
}

func TestParse_OnlyWhitespace(t *testing.T) {
	_, err := Parse(strings.NewReader("   \n\t  "))
	if err == nil {
		t.Error("expected error for whitespace-only input")
	}
}

func TestParse_ValidXMLButNotNZB(t *testing.T) {
	_, err := Parse(strings.NewReader(`<?xml version="1.0"?><html><body>Hello</body></html>`))
	if err == nil {
		t.Error("expected error for non-NZB XML")
	}
}

func TestParse_NZBWithNoSegments(t *testing.T) {
	xml := `<?xml version="1.0"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file poster="test" date="0" subject="&quot;test.bin&quot;">
    <groups><group>alt.test</group></groups>
    <segments></segments>
  </file>
</nzb>`
	_, err := Parse(strings.NewReader(xml))
	if err == nil {
		t.Error("expected error for file with no segments")
	}
}

func TestParse_SegmentWithEmptyMessageID(t *testing.T) {
	xml := `<?xml version="1.0"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file poster="test" date="0" subject="&quot;test.bin&quot;">
    <groups><group>alt.test</group></groups>
    <segments>
      <segment bytes="100" number="1">   </segment>
    </segments>
  </file>
</nzb>`
	_, err := Parse(strings.NewReader(xml))
	if err == nil {
		t.Error("expected error: segment with empty/whitespace message ID should be skipped, leaving no valid files")
	}
}

func TestParse_MixedValidAndEmptySegments(t *testing.T) {
	xml := `<?xml version="1.0"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file poster="test" date="0" subject="&quot;test.bin&quot;">
    <groups><group>alt.test</group></groups>
    <segments>
      <segment bytes="100" number="1">valid@id</segment>
      <segment bytes="200" number="2">  </segment>
      <segment bytes="300" number="3">also-valid@id</segment>
    </segments>
  </file>
</nzb>`
	nzb, err := Parse(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(nzb.Files[0].Segments) != 2 {
		t.Errorf("expected 2 valid segments, got %d", len(nzb.Files[0].Segments))
	}
}

// --- Metadata / Head parsing ---

func TestParse_MetaPassword(t *testing.T) {
	xml := `<?xml version="1.0"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <head>
    <meta type="password">s3cr3t</meta>
    <meta type="title">My Movie</meta>
    <meta type="category">Movies</meta>
  </head>
  <file poster="test" date="0" subject="&quot;test.bin&quot;">
    <groups><group>alt.test</group></groups>
    <segments>
      <segment bytes="100" number="1">seg@id</segment>
    </segments>
  </file>
</nzb>`
	nzb, err := Parse(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if nzb.Password != "s3cr3t" {
		t.Errorf("Password: got %q, want %q", nzb.Password, "s3cr3t")
	}
	if nzb.Meta["title"] != "My Movie" {
		t.Errorf("Meta title: got %q", nzb.Meta["title"])
	}
	if nzb.Meta["category"] != "Movies" {
		t.Errorf("Meta category: got %q", nzb.Meta["category"])
	}
}

func TestParse_MetaPasswordWithWhitespace(t *testing.T) {
	xml := `<?xml version="1.0"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <head>
    <meta type="password">  padded  </meta>
  </head>
  <file poster="test" date="0" subject="&quot;test.bin&quot;">
    <groups><group>alt.test</group></groups>
    <segments>
      <segment bytes="100" number="1">seg@id</segment>
    </segments>
  </file>
</nzb>`
	nzb, err := Parse(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if nzb.Password != "padded" {
		t.Errorf("Password should be trimmed: got %q", nzb.Password)
	}
}

func TestParse_NoHead(t *testing.T) {
	xml := `<?xml version="1.0"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file poster="test" date="0" subject="&quot;test.bin&quot;">
    <groups><group>alt.test</group></groups>
    <segments>
      <segment bytes="100" number="1">seg@id</segment>
    </segments>
  </file>
</nzb>`
	nzb, err := Parse(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if nzb.Password != "" {
		t.Errorf("Password should be empty: got %q", nzb.Password)
	}
	if len(nzb.Meta) != 0 {
		t.Errorf("Meta should be empty: got %v", nzb.Meta)
	}
}

func TestParse_MetaWithEmptyType(t *testing.T) {
	xml := `<?xml version="1.0"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <head>
    <meta type="">ignored</meta>
    <meta type="name">kept</meta>
  </head>
  <file poster="test" date="0" subject="&quot;test.bin&quot;">
    <groups><group>alt.test</group></groups>
    <segments>
      <segment bytes="100" number="1">seg@id</segment>
    </segments>
  </file>
</nzb>`
	nzb, err := Parse(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if _, ok := nzb.Meta[""]; ok {
		t.Error("empty-type meta should not be stored")
	}
	if nzb.Meta["name"] != "kept" {
		t.Errorf("Meta name: got %q", nzb.Meta["name"])
	}
}

// --- Multiple files ---

func TestParse_MultipleFilesVariousTypes(t *testing.T) {
	xml := `<?xml version="1.0"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file poster="bot" date="1700000000" subject="&quot;movie.mkv&quot; yEnc (1/100)">
    <groups><group>alt.binaries.movies</group></groups>
    <segments>
      <segment bytes="768000" number="1">mkv001@ex</segment>
      <segment bytes="768000" number="2">mkv002@ex</segment>
    </segments>
  </file>
  <file poster="bot" date="1700000000" subject="&quot;movie.nfo&quot; yEnc (1/1)">
    <groups><group>alt.binaries.movies</group></groups>
    <segments>
      <segment bytes="4096" number="1">nfo001@ex</segment>
    </segments>
  </file>
  <file poster="bot" date="1700000000" subject="&quot;movie.par2&quot; yEnc (1/1)">
    <groups><group>alt.binaries.movies</group></groups>
    <segments>
      <segment bytes="32768" number="1">par001@ex</segment>
    </segments>
  </file>
  <file poster="bot" date="1700000000" subject="&quot;movie.vol0+1.par2&quot; yEnc (1/1)">
    <groups><group>alt.binaries.movies</group></groups>
    <segments>
      <segment bytes="65536" number="1">parv001@ex</segment>
    </segments>
  </file>
  <file poster="bot" date="1700000000" subject="&quot;sample.mkv&quot; yEnc (1/1)">
    <groups><group>alt.binaries.movies</group></groups>
    <segments>
      <segment bytes="10000" number="1">sample001@ex</segment>
    </segments>
  </file>
</nzb>`
	nzb, err := Parse(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if len(nzb.Files) != 5 {
		t.Fatalf("expected 5 files, got %d", len(nzb.Files))
	}

	// ContentFiles should exclude nfo, par2, par2 vol, and sample
	content := nzb.ContentFiles()
	if len(content) != 1 {
		t.Errorf("ContentFiles: got %d, want 1", len(content))
	}
	if len(content) > 0 && content[0].Filename() != "movie.mkv" {
		t.Errorf("ContentFiles[0]: got %q, want movie.mkv", content[0].Filename())
	}

	// Par2Files
	par2 := nzb.Par2Files()
	if len(par2) != 2 {
		t.Errorf("Par2Files: got %d, want 2", len(par2))
	}

	if !nzb.HasPar2() {
		t.Error("HasPar2 should be true")
	}
	if nzb.HasRars() {
		t.Error("HasRars should be false for this NZB")
	}
}

// --- Segment ordering / number parsing ---

func TestParse_SegmentNumberParsing(t *testing.T) {
	xml := `<?xml version="1.0"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file poster="test" date="0" subject="&quot;test.bin&quot;">
    <groups><group>alt.test</group></groups>
    <segments>
      <segment bytes="100" number="3">c@id</segment>
      <segment bytes="200" number="1">a@id</segment>
      <segment bytes="300" number="2">b@id</segment>
    </segments>
  </file>
</nzb>`
	nzb, err := Parse(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	segs := nzb.Files[0].Segments
	if len(segs) != 3 {
		t.Fatalf("expected 3 segments, got %d", len(segs))
	}

	// Parse preserves order from XML; sorting is done by the downloader
	// Verify numbers are parsed correctly
	numbers := make(map[int]bool)
	for _, s := range segs {
		numbers[s.Number] = true
	}
	for _, want := range []int{1, 2, 3} {
		if !numbers[want] {
			t.Errorf("missing segment number %d", want)
		}
	}
}

func TestParse_SegmentBytesZero(t *testing.T) {
	xml := `<?xml version="1.0"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file poster="test" date="0" subject="&quot;test.bin&quot;">
    <groups><group>alt.test</group></groups>
    <segments>
      <segment bytes="0" number="1">seg@id</segment>
    </segments>
  </file>
</nzb>`
	nzb, err := Parse(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if nzb.Files[0].Segments[0].Bytes != 0 {
		t.Errorf("expected 0 bytes, got %d", nzb.Files[0].Segments[0].Bytes)
	}
}

func TestParse_SegmentBytesNonNumeric(t *testing.T) {
	xml := `<?xml version="1.0"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file poster="test" date="0" subject="&quot;test.bin&quot;">
    <groups><group>alt.test</group></groups>
    <segments>
      <segment bytes="abc" number="1">seg@id</segment>
    </segments>
  </file>
</nzb>`
	nzb, err := Parse(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	// Non-numeric bytes should parse as 0
	if nzb.Files[0].Segments[0].Bytes != 0 {
		t.Errorf("non-numeric bytes should be 0, got %d", nzb.Files[0].Segments[0].Bytes)
	}
}

// --- File helper methods ---

func TestFileTotalBytes(t *testing.T) {
	f := File{
		Segments: []Segment{
			{Bytes: 100}, {Bytes: 200}, {Bytes: 300},
		},
	}
	if got := f.TotalBytes(); got != 600 {
		t.Errorf("TotalBytes: got %d, want 600", got)
	}
}

func TestFileTotalBytes_Empty(t *testing.T) {
	f := File{}
	if got := f.TotalBytes(); got != 0 {
		t.Errorf("TotalBytes of empty file: got %d, want 0", got)
	}
}

func TestFileExtension_Various(t *testing.T) {
	tests := []struct {
		subject string
		want    string
	}{
		{`"file.MKV" yEnc`, ".mkv"},
		{`"file.RAR" yEnc`, ".rar"},
		{`"file.Par2" yEnc`, ".par2"},
		{`"noext" yEnc`, ""},
		{`"file.tar.gz" yEnc`, ".gz"},
	}
	for _, tt := range tests {
		f := File{Subject: tt.subject}
		if got := f.Extension(); got != tt.want {
			t.Errorf("Extension(%q) = %q, want %q", tt.subject, got, tt.want)
		}
	}
}

// --- LargestFile edge cases ---

func TestLargestFile_EmptyNZB(t *testing.T) {
	nzb := &NZB{}
	if nzb.LargestFile() != nil {
		t.Error("LargestFile should return nil for empty NZB")
	}
}

func TestLargestFile_SingleFile(t *testing.T) {
	nzb := &NZB{
		Files: []File{
			{Subject: `"only.bin"`, Segments: []Segment{{Bytes: 100}}},
		},
	}
	largest := nzb.LargestFile()
	if largest == nil {
		t.Fatal("LargestFile should not be nil")
	}
	if largest.Filename() != "only.bin" {
		t.Errorf("got %q", largest.Filename())
	}
}

func TestLargestFile_MultipleSameSize(t *testing.T) {
	nzb := &NZB{
		Files: []File{
			{Subject: `"a.bin"`, Segments: []Segment{{Bytes: 100}}},
			{Subject: `"b.bin"`, Segments: []Segment{{Bytes: 100}}},
		},
	}
	largest := nzb.LargestFile()
	if largest == nil {
		t.Fatal("LargestFile should not be nil")
	}
	// Should return the first one (stable)
	if largest.Filename() != "a.bin" {
		t.Errorf("got %q, expected first file for equal sizes", largest.Filename())
	}
}

// --- IsObfuscated ---

func TestIsObfuscated_Normal(t *testing.T) {
	nzb := &NZB{
		Files: []File{
			{Subject: `"Movie.2024.1080p.BluRay.x264-GROUP.mkv"`},
		},
	}
	if nzb.IsObfuscated() {
		t.Error("normal filename should not be obfuscated")
	}
}

func TestIsObfuscated_HexName(t *testing.T) {
	nzb := &NZB{
		Files: []File{
			{Subject: `"a1b2c3d4e5f6a7b8c9d0e1f2.mkv"`},
		},
	}
	if !nzb.IsObfuscated() {
		t.Error("hex-like filename should be obfuscated")
	}
}

func TestIsObfuscated_EmptyFiles(t *testing.T) {
	nzb := &NZB{}
	if nzb.IsObfuscated() {
		t.Error("empty NZB should not be obfuscated")
	}
}

func TestIsObfuscated_ShortHex(t *testing.T) {
	// Short name (<=10 chars) should not trigger obfuscation
	nzb := &NZB{
		Files: []File{
			{Subject: `"abcdef.mkv"`},
		},
	}
	if nzb.IsObfuscated() {
		t.Error("short hex-like name should not be obfuscated")
	}
}

// --- isMetadataFile ---

func TestIsMetadataFile(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"file.par2", true},
		{"file.nfo", true},
		{"file.sfv", true},
		{"file.nzb", true},
		{"file.txt", true},
		{"file.jpg", true},
		{"file.png", true},
		{"file.url", true},
		{"file.mkv", false},
		{"file.rar", false},
		{"file.avi", false},
		{"FILE.PAR2", true},
		{"FILE.NFO", true},
	}
	for _, tt := range tests {
		if got := isMetadataFile(tt.name); got != tt.want {
			t.Errorf("isMetadataFile(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// --- isSampleFile ---

func TestIsSampleFile(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"movie.sample.mkv", true},
		{"Sample.mkv", true},
		{"SAMPLE.avi", true},
		{"movie-sample-video.mkv", true},
		{"movie_sample.mkv", true},
		{"sample.mkv", true},
		{"resampled.mkv", false}, // "sample" is part of "resampled"
		{"movie.mkv", false},
		{"my.samples.zip", false}, // "sample" followed by 's' (alphanumeric)
	}
	for _, tt := range tests {
		if got := isSampleFile(tt.name); got != tt.want {
			t.Errorf("isSampleFile(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// --- isHexLike ---

func TestIsHexLike(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"abcdef0123456789", true},
		{"ABCDEF", true},
		{"Movie2024", false},
		{"aabbccdd", true},
		{"xyz_not_hex", false},
	}
	for _, tt := range tests {
		if got := isHexLike(tt.input); got != tt.want {
			t.Errorf("isHexLike(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

// --- sanitizeFilename ---

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"simple name", "simple name"},
		{"name (1/50)", "name"},
		{"file yEnc (01/99)", "file"},
		{`path/with\special:chars*?`, `path_with_special_chars__`},
		{`"quoted" text`, `_quoted_ text`},
		{"  spaces  ", "spaces"},
	}
	for _, tt := range tests {
		if got := sanitizeFilename(tt.input); got != tt.want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// --- Filename fallback ---

func TestFilename_Fallback_NoQuotes(t *testing.T) {
	f := File{Subject: "No quotes here yEnc (1/50)"}
	got := f.Filename()
	if got != "No quotes here" {
		t.Errorf("Filename fallback: got %q, want %q", got, "No quotes here")
	}
}

func TestFilename_EmptySubject(t *testing.T) {
	f := File{Subject: ""}
	got := f.Filename()
	if got != "" {
		t.Errorf("Filename empty subject: got %q, want empty", got)
	}
}

// --- NZB aggregate methods on mixed content ---

func TestNZB_HasRars_NoRars(t *testing.T) {
	nzb := &NZB{
		Files: []File{
			{Subject: `"movie.mkv"`},
			{Subject: `"movie.par2"`},
		},
	}
	if nzb.HasRars() {
		t.Error("HasRars should be false")
	}
}

func TestNZB_HasPar2_NoPar2(t *testing.T) {
	nzb := &NZB{
		Files: []File{
			{Subject: `"movie.mkv"`},
			{Subject: `"movie.rar"`},
		},
	}
	if nzb.HasPar2() {
		t.Error("HasPar2 should be false")
	}
}

func TestNZB_TotalSegments_MultiFile(t *testing.T) {
	nzb := &NZB{
		Files: []File{
			{Segments: []Segment{{}, {}, {}}},
			{Segments: []Segment{{}, {}}},
		},
	}
	if got := nzb.TotalSegments(); got != 5 {
		t.Errorf("TotalSegments: got %d, want 5", got)
	}
}

func TestNZB_TotalBytes_MultiFile(t *testing.T) {
	nzb := &NZB{
		Files: []File{
			{Segments: []Segment{{Bytes: 100}, {Bytes: 200}}},
			{Segments: []Segment{{Bytes: 300}}},
		},
	}
	if got := nzb.TotalBytes(); got != 600 {
		t.Errorf("TotalBytes: got %d, want 600", got)
	}
}

// --- isRarFile extended ---

func TestIsRarFile_Extended(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"file.RAR", true}, // case insensitive
		{"file.Rar", true},
		{"file.s01", true},
		{"file.s99", true},
		{"file.002", true},
		{"file.999", true},
		{"file.r0", false},  // too short extension
		{"file.rXX", false}, // non-numeric
		{"file", false},     // no extension
		{"file.mp4", false},
	}
	for _, tt := range tests {
		if got := isRarFile(tt.name); got != tt.want {
			t.Errorf("isRarFile(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// --- Parse with date edge cases ---

func TestParse_DateNonNumeric(t *testing.T) {
	xml := `<?xml version="1.0"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file poster="test" date="not-a-number" subject="&quot;test.bin&quot;">
    <groups><group>alt.test</group></groups>
    <segments>
      <segment bytes="100" number="1">seg@id</segment>
    </segments>
  </file>
</nzb>`
	nzb, err := Parse(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if nzb.Files[0].Date != 0 {
		t.Errorf("non-numeric date should be 0, got %d", nzb.Files[0].Date)
	}
}

func TestParse_DateEmpty(t *testing.T) {
	xml := `<?xml version="1.0"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file poster="test" date="" subject="&quot;test.bin&quot;">
    <groups><group>alt.test</group></groups>
    <segments>
      <segment bytes="100" number="1">seg@id</segment>
    </segments>
  </file>
</nzb>`
	nzb, err := Parse(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if nzb.Files[0].Date != 0 {
		t.Errorf("empty date should be 0, got %d", nzb.Files[0].Date)
	}
}

// --- Parse: file with all segments having empty IDs should be excluded ---

func TestParse_AllEmptySegments(t *testing.T) {
	xml := `<?xml version="1.0"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file poster="test" date="0" subject="&quot;bad.bin&quot;">
    <groups><group>alt.test</group></groups>
    <segments>
      <segment bytes="100" number="1">  </segment>
      <segment bytes="200" number="2"></segment>
    </segments>
  </file>
  <file poster="test" date="0" subject="&quot;good.bin&quot;">
    <groups><group>alt.test</group></groups>
    <segments>
      <segment bytes="100" number="1">valid@id</segment>
    </segments>
  </file>
</nzb>`
	nzb, err := Parse(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(nzb.Files) != 1 {
		t.Fatalf("expected 1 valid file, got %d", len(nzb.Files))
	}
	if nzb.Files[0].Filename() != "good.bin" {
		t.Errorf("expected good.bin, got %q", nzb.Files[0].Filename())
	}
}

// --- Groups ---

func TestParse_NoGroups(t *testing.T) {
	xml := `<?xml version="1.0"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file poster="test" date="0" subject="&quot;test.bin&quot;">
    <groups></groups>
    <segments>
      <segment bytes="100" number="1">seg@id</segment>
    </segments>
  </file>
</nzb>`
	nzb, err := Parse(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(nzb.Files[0].Groups) != 0 {
		t.Errorf("expected 0 groups, got %d", len(nzb.Files[0].Groups))
	}
}

func TestParse_MultipleGroups(t *testing.T) {
	xml := `<?xml version="1.0"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file poster="test" date="0" subject="&quot;test.bin&quot;">
    <groups>
      <group>alt.binaries.movies</group>
      <group>alt.binaries.multimedia</group>
      <group>alt.binaries.hdtv</group>
    </groups>
    <segments>
      <segment bytes="100" number="1">seg@id</segment>
    </segments>
  </file>
</nzb>`
	nzb, err := Parse(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(nzb.Files[0].Groups) != 3 {
		t.Errorf("expected 3 groups, got %d", len(nzb.Files[0].Groups))
	}
}

// --- ContentFiles with sample variations ---

func TestContentFiles_ExcludesSamples(t *testing.T) {
	nzb := &NZB{
		Files: []File{
			{Subject: `"movie.mkv"`, Segments: []Segment{{Bytes: 1000, MessageID: "a"}}},
			{Subject: `"movie.sample.mkv"`, Segments: []Segment{{Bytes: 100, MessageID: "b"}}},
			{Subject: `"Sample/preview.mkv"`, Segments: []Segment{{Bytes: 100, MessageID: "c"}}},
		},
	}
	content := nzb.ContentFiles()
	if len(content) != 1 {
		t.Errorf("ContentFiles should exclude samples: got %d, want 1", len(content))
	}
}

// --- RarFiles with split naming ---

func TestRarFiles_SplitRars(t *testing.T) {
	nzb := &NZB{
		Files: []File{
			{Subject: `"movie.rar"`, Segments: []Segment{{MessageID: "a"}}},
			{Subject: `"movie.r00"`, Segments: []Segment{{MessageID: "b"}}},
			{Subject: `"movie.r01"`, Segments: []Segment{{MessageID: "c"}}},
			{Subject: `"movie.001"`, Segments: []Segment{{MessageID: "d"}}},
			{Subject: `"movie.002"`, Segments: []Segment{{MessageID: "e"}}},
			{Subject: `"movie.par2"`, Segments: []Segment{{MessageID: "f"}}},
			{Subject: `"movie.mkv"`, Segments: []Segment{{MessageID: "g"}}},
		},
	}
	rars := nzb.RarFiles()
	if len(rars) != 5 {
		t.Errorf("RarFiles: got %d, want 5", len(rars))
	}
}

package nzb

import (
	"encoding/xml"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// NZB represents a parsed NZB file containing one or more files to download.
type NZB struct {
	Files    []File
	Password string            // from <meta type="password"> in <head>
	Meta     map[string]string // all <meta> entries from <head>
}

// File represents a single file within an NZB, composed of multiple segments.
type File struct {
	Poster   string
	Date     int64
	Subject  string
	Groups   []string
	Segments []Segment
}

// Segment represents a single NNTP article segment of a file.
type Segment struct {
	Bytes     int64
	Number    int
	MessageID string // message-id without angle brackets
}

// xmlNZB is the raw XML structure for parsing.
type xmlNZB struct {
	XMLName xml.Name  `xml:"nzb"`
	Head    xmlHead   `xml:"head"`
	Files   []xmlFile `xml:"file"`
}

type xmlHead struct {
	Meta []xmlMeta `xml:"meta"`
}

type xmlMeta struct {
	Type  string `xml:"type,attr"`
	Value string `xml:",chardata"`
}

type xmlFile struct {
	Poster   string        `xml:"poster,attr"`
	Date     string        `xml:"date,attr"`
	Subject  string        `xml:"subject,attr"`
	Groups   xmlGroups     `xml:"groups"`
	Segments xmlSegments   `xml:"segments"`
}

type xmlGroups struct {
	Groups []string `xml:"group"`
}

type xmlSegments struct {
	Segments []xmlSegment `xml:"segment"`
}

type xmlSegment struct {
	Bytes     string `xml:"bytes,attr"`
	Number    string `xml:"number,attr"`
	MessageID string `xml:",chardata"`
}

// Parse reads and parses an NZB XML document from the given reader.
func Parse(r io.Reader) (*NZB, error) {
	var raw xmlNZB
	dec := xml.NewDecoder(r)
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("nzb: xml decode: %w", err)
	}

	if len(raw.Files) == 0 {
		return nil, fmt.Errorf("nzb: no files found")
	}

	nzb := &NZB{
		Files: make([]File, 0, len(raw.Files)),
		Meta:  make(map[string]string),
	}

	// Parse <head> meta entries
	for _, m := range raw.Head.Meta {
		if m.Type != "" {
			nzb.Meta[m.Type] = strings.TrimSpace(m.Value)
		}
	}
	nzb.Password = nzb.Meta["password"]

	for _, rf := range raw.Files {
		date, _ := strconv.ParseInt(rf.Date, 10, 64)

		segs := make([]Segment, 0, len(rf.Segments.Segments))
		for _, rs := range rf.Segments.Segments {
			bytes, _ := strconv.ParseInt(rs.Bytes, 10, 64)
			num, _ := strconv.Atoi(rs.Number)
			msgID := strings.TrimSpace(rs.MessageID)
			// Strip angle brackets if present
			msgID = strings.TrimPrefix(msgID, "<")
			msgID = strings.TrimSuffix(msgID, ">")

			if msgID == "" {
				continue
			}

			segs = append(segs, Segment{
				Bytes:     bytes,
				Number:    num,
				MessageID: msgID,
			})
		}

		if len(segs) == 0 {
			continue
		}

		nzb.Files = append(nzb.Files, File{
			Poster:   rf.Poster,
			Date:     date,
			Subject:  rf.Subject,
			Groups:   rf.Groups.Groups,
			Segments: segs,
		})
	}

	if len(nzb.Files) == 0 {
		return nil, fmt.Errorf("nzb: no valid files with segments found")
	}

	return nzb, nil
}

// ParseBytes parses an NZB from a byte slice.
func ParseBytes(data []byte) (*NZB, error) {
	return Parse(strings.NewReader(string(data)))
}

// TotalBytes returns the total size of all segments across all files.
func (n *NZB) TotalBytes() int64 {
	var total int64
	for _, f := range n.Files {
		total += f.TotalBytes()
	}
	return total
}

// TotalSegments returns the total number of segments across all files.
func (n *NZB) TotalSegments() int {
	var total int
	for _, f := range n.Files {
		total += len(f.Segments)
	}
	return total
}

// ContentFiles returns files that are likely content (video, audio, images),
// excluding par2, nfo, sfv, nzb, and sample files.
func (n *NZB) ContentFiles() []File {
	var result []File
	for _, f := range n.Files {
		name := f.Filename()
		if isMetadataFile(name) || isSampleFile(name) {
			continue
		}
		result = append(result, f)
	}
	return result
}

// Par2Files returns only par2 parity files.
func (n *NZB) Par2Files() []File {
	var result []File
	for _, f := range n.Files {
		ext := strings.ToLower(filepath.Ext(f.Filename()))
		if ext == ".par2" {
			result = append(result, f)
		}
	}
	return result
}

// RarFiles returns rar archive files (.rar, .rNN, .NNN).
func (n *NZB) RarFiles() []File {
	var result []File
	for _, f := range n.Files {
		if isRarFile(f.Filename()) {
			result = append(result, f)
		}
	}
	return result
}

// LargestFile returns the file with the most total bytes.
// Returns nil if NZB has no files.
func (n *NZB) LargestFile() *File {
	if len(n.Files) == 0 {
		return nil
	}
	largest := &n.Files[0]
	for i := 1; i < len(n.Files); i++ {
		if n.Files[i].TotalBytes() > largest.TotalBytes() {
			largest = &n.Files[i]
		}
	}
	return largest
}

// IsObfuscated returns true if the NZB filenames appear to be obfuscated
// (random strings instead of meaningful names).
func (n *NZB) IsObfuscated() bool {
	for _, f := range n.Files {
		name := f.Filename()
		if name == "" {
			continue
		}
		base := strings.TrimSuffix(name, filepath.Ext(name))
		// Check if base name is mostly hex/random chars (obfuscated)
		if len(base) > 10 && isHexLike(base) {
			return true
		}
	}
	return false
}

// HasRars returns true if the NZB contains rar archive files.
func (n *NZB) HasRars() bool {
	for _, f := range n.Files {
		if isRarFile(f.Filename()) {
			return true
		}
	}
	return false
}

// HasPar2 returns true if the NZB contains par2 parity files.
func (n *NZB) HasPar2() bool {
	for _, f := range n.Files {
		ext := strings.ToLower(filepath.Ext(f.Filename()))
		if ext == ".par2" {
			return true
		}
	}
	return false
}

// TotalBytes returns the sum of all segment sizes in this file.
func (f *File) TotalBytes() int64 {
	var total int64
	for _, s := range f.Segments {
		total += s.Bytes
	}
	return total
}

// subjectFilenameRe matches the filename in a typical Usenet subject line.
// Examples:
//   "Movie.2024.1080p.mkv" yEnc (1/50)
//   [PRiVATE]-[#a]- "file.rar" yEnc (01/99)
var subjectFilenameRe = regexp.MustCompile(`"([^"]+)"`)

// Filename extracts the filename from the subject line.
// Falls back to the raw subject if no quoted filename is found.
func (f *File) Filename() string {
	m := subjectFilenameRe.FindStringSubmatch(f.Subject)
	if len(m) >= 2 {
		return m[1]
	}
	// Fallback: try to extract something useful
	return sanitizeFilename(f.Subject)
}

// Extension returns the lowercase file extension (e.g., ".mkv", ".rar").
func (f *File) Extension() string {
	return strings.ToLower(filepath.Ext(f.Filename()))
}

// isMetadataFile returns true for non-content files.
func isMetadataFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".par2", ".nfo", ".sfv", ".nzb", ".txt", ".jpg", ".png", ".url":
		return true
	}
	return false
}

// isSampleFile returns true for sample/preview files.
// Matches filenames containing "sample" as a word boundary (e.g., "movie.sample.mkv", "Sample/video.mkv").
func isSampleFile(name string) bool {
	lower := strings.ToLower(name)
	// Match "sample" preceded and followed by non-alphanumeric (word boundary)
	idx := strings.Index(lower, "sample")
	if idx < 0 {
		return false
	}
	// Check it's not part of a larger word (e.g., "resampled")
	if idx > 0 && isAlphaNum(lower[idx-1]) {
		return false
	}
	end := idx + 6
	if end < len(lower) && isAlphaNum(lower[end]) {
		return false
	}
	return true
}

func isAlphaNum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

// isRarFile returns true for rar archive files.
func isRarFile(name string) bool {
	lower := strings.ToLower(name)
	ext := filepath.Ext(lower)
	if ext == ".rar" {
		return true
	}
	// Match .r00, .r01, ..., .r99 and .s00, .s01
	if len(ext) == 4 && (ext[1] == 'r' || ext[1] == 's') {
		_, err := strconv.Atoi(ext[2:])
		return err == nil
	}
	// Match .001, .002, etc (split rar)
	if len(ext) == 4 {
		_, err := strconv.Atoi(ext[1:])
		return err == nil
	}
	return false
}

// isHexLike returns true if the string looks like random hex/obfuscated.
func isHexLike(s string) bool {
	hexChars := 0
	for _, c := range s {
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			hexChars++
		}
	}
	return float64(hexChars)/float64(len(s)) > 0.8
}

var yencPartRe = regexp.MustCompile(`\s*\(\d+/\d+\)\s*`)

// sanitizeFilename removes characters that are invalid in filenames.
func sanitizeFilename(s string) string {
	// Remove yEnc part indicators like (01/50)
	s = yencPartRe.ReplaceAllString(s, "")
	// Remove yEnc keyword
	s = strings.ReplaceAll(s, "yEnc", "")
	s = strings.TrimSpace(s)
	// Remove invalid path chars
	for _, c := range []string{"/", "\\", ":", "*", "?", "\"", "<", ">", "|"} {
		s = strings.ReplaceAll(s, c, "_")
	}
	return s
}

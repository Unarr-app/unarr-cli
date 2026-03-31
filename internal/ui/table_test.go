package ui

import (
	"bytes"
	"strings"
	"testing"

	tc "github.com/torrentclaw/go-client"
)

func TestNewCleanTable(t *testing.T) {
	var buf bytes.Buffer
	tbl := newCleanTable(&buf)
	tbl.Header([]string{"A", "B"})
	tbl.Append([]string{"1", "2"})
	tbl.Render()

	out := buf.String()
	if !strings.Contains(out, "A") || !strings.Contains(out, "B") {
		t.Errorf("expected headers in output, got: %s", out)
	}
	if !strings.Contains(out, "1") || !strings.Contains(out, "2") {
		t.Errorf("expected row data in output, got: %s", out)
	}
}

func TestPrintSearchResultEntry(t *testing.T) {
	var buf bytes.Buffer
	year := 2010
	rating := "8.8"
	quality := "1080p"
	codec := "x265"
	size := int64(4294967296)
	score := 85

	r := tc.SearchResult{
		Title:       "Inception",
		Year:        &year,
		ContentType: "movie",
		RatingIMDb:  &rating,
		Genres:      []string{"Sci-Fi", "Action"},
		Torrents: []tc.TorrentInfo{
			{
				Quality:      &quality,
				SizeBytes:    &size,
				Seeders:      150,
				Leechers:     10,
				Source:       "YTS",
				Codec:        &codec,
				Languages:    []string{"en", "es"},
				QualityScore: &score,
			},
		},
	}

	printSearchResultEntry(&buf, r)
	out := buf.String()

	if !strings.Contains(out, "Inception") {
		t.Error("expected title in output")
	}
	if !strings.Contains(out, "2010") {
		t.Error("expected year in output")
	}
	if !strings.Contains(out, "8.8") {
		t.Error("expected rating in output")
	}
	if !strings.Contains(out, "1080p") {
		t.Error("expected quality in output")
	}
	if !strings.Contains(out, "YTS") {
		t.Error("expected source in output")
	}
}

func TestPrintSearchResultEntryNoTorrents(t *testing.T) {
	var buf bytes.Buffer
	year := 2020

	r := tc.SearchResult{
		Title:       "No Torrents Movie",
		Year:        &year,
		ContentType: "movie",
		Torrents:    nil,
	}

	printSearchResultEntry(&buf, r)
	out := buf.String()

	if !strings.Contains(out, "No Torrents Movie") {
		t.Error("expected title in output")
	}
	if !strings.Contains(out, "No torrents available") {
		t.Error("expected no-torrents message")
	}
}

func TestPrintSearchResultEntryNilFields(t *testing.T) {
	var buf bytes.Buffer

	r := tc.SearchResult{
		Title:       "Minimal",
		ContentType: "movie",
		Torrents: []tc.TorrentInfo{
			{
				Seeders:  5,
				Leechers: 3,
				Source:   "RARBG",
			},
		},
	}

	printSearchResultEntry(&buf, r)
	out := buf.String()

	if !strings.Contains(out, "Minimal") {
		t.Error("expected title in output")
	}
	if !strings.Contains(out, "-") {
		t.Error("expected dash for nil fields")
	}
}

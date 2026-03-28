package parser

import (
	"strings"
	"testing"
)

func TestParseMagnet(t *testing.T) {
	magnet := "magnet:?xt=urn:btih:ABC123DEF456ABC123DEF456ABC123DEF456ABC1&dn=Oppenheimer.2023.1080p.BluRay.x265"
	p := Parse(magnet)

	if !p.IsMagnet {
		t.Error("expected IsMagnet=true")
	}
	if p.InfoHash != "abc123def456abc123def456abc123def456abc1" {
		t.Errorf("InfoHash = %q, want lowercase 40-char hash", p.InfoHash)
	}
	if p.Quality != "1080p" {
		t.Errorf("Quality = %q, want 1080p", p.Quality)
	}
	if p.Codec != "x265" {
		t.Errorf("Codec = %q, want x265", p.Codec)
	}
	if p.Year != "2023" {
		t.Errorf("Year = %q, want 2023", p.Year)
	}
}

func TestParseInfoHash(t *testing.T) {
	hash := "abc123def456abc123def456abc123def456abc1" // exactly 40 hex chars
	p := Parse(hash)

	if p.IsMagnet {
		t.Error("expected IsMagnet=false for plain hash")
	}
	if p.InfoHash != hash {
		t.Errorf("InfoHash = %q, want %q", p.InfoHash, hash)
	}
}

func TestParseName(t *testing.T) {
	tests := []struct {
		input   string
		quality string
		codec   string
		year    string
	}{
		{"The.Matrix.1999.1080p.BluRay.x264", "1080p", "x264", "1999"},
		{"Oppenheimer.2023.2160p.UHD.BluRay.x265", "2160p", "x265", "2023"},
		{"Movie.720p.HDTV.HEVC", "720p", "HEVC", ""},
		{"Show.480p.WEB.AV1", "480p", "AV1", ""},
		{"No.Quality.Info.Here", "", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			p := Parse(tt.input)
			if p.Quality != tt.quality {
				t.Errorf("Quality = %q, want %q", p.Quality, tt.quality)
			}
			if p.Codec != tt.codec {
				t.Errorf("Codec = %q, want %q", p.Codec, tt.codec)
			}
			if p.Year != tt.year {
				t.Errorf("Year = %q, want %q", p.Year, tt.year)
			}
		})
	}
}

func TestExtractSearchQuery(t *testing.T) {
	tests := []struct {
		input string
	}{
		{"The.Matrix.1999.1080p.BluRay.x264-GROUP"},
		{"Oppenheimer.2023.2160p.UHD.BluRay.x265.DTS-HD"},
		{"Breaking.Bad.S01E01.720p.WEB-DL"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := ExtractSearchQuery(tt.input)
			if got == "" {
				t.Errorf("ExtractSearchQuery(%q) returned empty string", tt.input)
			}
			// Should not contain quality/codec artifacts
			if strings.Contains(got, "1080p") || strings.Contains(got, "2160p") || strings.Contains(got, "720p") {
				t.Errorf("ExtractSearchQuery(%q) = %q, should not contain resolution", tt.input, got)
			}
			if strings.Contains(got, "x264") || strings.Contains(got, "x265") {
				t.Errorf("ExtractSearchQuery(%q) = %q, should not contain codec", tt.input, got)
			}
			if strings.Contains(got, "BluRay") {
				t.Errorf("ExtractSearchQuery(%q) = %q, should not contain source", tt.input, got)
			}
		})
	}
}

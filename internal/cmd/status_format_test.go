package cmd

import (
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"}, // last value before the KB boundary
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1024 * 1024, "1.0 MB"},
		{1024 * 1024 * 1024, "1.0 GB"},
		{1024 * 1024 * 1024 * 1024, "1.0 TB"},
	}
	for _, tt := range tests {
		if got := formatBytes(tt.in); got != tt.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{-5 * time.Second, "0s"}, // negative clamps to 0s
		{0, "0s"},
		{45 * time.Second, "45s"},            // < 1m
		{90 * time.Second, "1m 30s"},         // < 1h
		{time.Hour + 2*time.Minute, "1h 2m"}, // < 1d
		{25 * time.Hour, "1d 1h"},            // >= 1d
	}
	for _, tt := range tests {
		if got := formatDuration(tt.in); got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFormatFeatures(t *testing.T) {
	tests := []struct {
		name string
		in   agent.FeatureFlags
		want string
	}{
		{"none", agent.FeatureFlags{}, ""},
		{"torrent only", agent.FeatureFlags{Torrent: true}, "Torrent"},
		{"debrid only", agent.FeatureFlags{Debrid: true}, "Debrid"},
		{"usenet only", agent.FeatureFlags{Usenet: true}, "Usenet"},
		{"torrent+debrid", agent.FeatureFlags{Torrent: true, Debrid: true}, "Torrent, Debrid"},
		{"debrid+usenet", agent.FeatureFlags{Debrid: true, Usenet: true}, "Debrid, Usenet"},
		{"all three", agent.FeatureFlags{Torrent: true, Debrid: true, Usenet: true}, "Torrent, Debrid, Usenet"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatFeatures(tt.in); got != tt.want {
				t.Errorf("formatFeatures(%+v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

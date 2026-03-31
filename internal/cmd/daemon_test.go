package cmd

import "testing"

func TestDeriveWSURL(t *testing.T) {
	tests := []struct {
		apiURL  string
		agentID string
		want    string
	}{
		{"https://torrentclaw.com", "agent-123", "wss://unarr.torrentclaw.com/ws/agent-123"},
		{"http://localhost:3000", "a1", ""},        // localhost skipped
		{"http://127.0.0.1:3000", "a1", ""},        // 127.0.0.1 skipped
		{"https://torrentclaw.com/", "a1", "wss://unarr.torrentclaw.com/ws/a1"},
		{"https://api.example.io", "x", "wss://unarr.api.example.io/ws/x"},
		{"", "agent-123", ""},
		{"https://torrentclaw.com", "", ""},
		{"", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.apiURL+"_"+tt.agentID, func(t *testing.T) {
			got := deriveWSURL(tt.apiURL, tt.agentID)
			if got != tt.want {
				t.Errorf("deriveWSURL(%q, %q) = %q, want %q", tt.apiURL, tt.agentID, got, tt.want)
			}
		})
	}
}

func TestFormatSpeedLog(t *testing.T) {
	tests := []struct {
		bps  int64
		want string
	}{
		{0, "0 B/s"},
		{500, "500 B/s"},
		{1023, "1023 B/s"},
		{1024, "1 KB/s"},
		{10240, "10 KB/s"},
		{1048576, "1.0 MB/s"},
		{5242880, "5.0 MB/s"},
		{1073741824, "1.0 GB/s"},
		{2147483648, "2.0 GB/s"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := formatSpeedLog(tt.bps)
			if got != tt.want {
				t.Errorf("formatSpeedLog(%d) = %q, want %q", tt.bps, got, tt.want)
			}
		})
	}
}

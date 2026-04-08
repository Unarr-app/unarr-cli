package cmd

import "testing"

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

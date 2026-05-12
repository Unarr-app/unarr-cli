package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseBitrateKbps(t *testing.T) {
	cases := []struct {
		in   string
		fb   int
		want int
	}{
		{"", 5000, 5000},
		{"192k", 0, 192},
		{"192K", 0, 192},
		{"5M", 0, 5000},
		{"5m", 0, 5000},
		{"4500", 0, 4500},
		{"bogus", 100, 100},
		{"0k", 100, 100},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := parseBitrateKbps(tc.in, tc.fb); got != tc.want {
				t.Errorf("parseBitrateKbps(%q,%d) = %d, want %d", tc.in, tc.fb, got, tc.want)
			}
		})
	}
}

func TestEstimateOutputSize(t *testing.T) {
	if got := estimateOutputSize(nil, TranscodeOpts{}); got != 0 {
		t.Errorf("nil probe -> 0, got %d", got)
	}
	if got := estimateOutputSize(&StreamProbe{}, TranscodeOpts{}); got != 0 {
		t.Errorf("zero duration -> 0, got %d", got)
	}
	probe := &StreamProbe{DurationSec: 60}
	opts := TranscodeOpts{VideoBitrate: "5M", AudioBitrate: "192k"}
	// (5000 + 192) * 1000 / 8 = 649_000 bytes/s; *60 = 38_940_000
	got := estimateOutputSize(probe, opts)
	if got != 38_940_000 {
		t.Errorf("estimateOutputSize = %d, want 38_940_000", got)
	}
}

func TestDiskFileSourceLifecycle(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "movie.bin")
	data := []byte("hello world")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	src, err := newDiskFileSource(path)
	if err != nil {
		t.Fatalf("newDiskFileSource: %v", err)
	}
	defer src.Close()

	if src.Size() != int64(len(data)) {
		t.Errorf("Size = %d, want %d", src.Size(), len(data))
	}
	if src.EstimatedSize() != src.Size() {
		t.Errorf("EstimatedSize should equal Size for disk source")
	}
	if !src.Final() {
		t.Errorf("disk source should be Final")
	}
	if src.Transcoded() {
		t.Errorf("disk source should not report Transcoded")
	}
	if src.FileName() != "movie.bin" {
		t.Errorf("FileName = %q", src.FileName())
	}

	buf := make([]byte, 5)
	n, err := src.ReadAt(buf, 6)
	if err != nil || n != 5 || string(buf) != "world" {
		t.Errorf("ReadAt = (%d,%v,%q), want (5,nil,'world')", n, err, buf)
	}
}

func TestDiskFileSourceMissing(t *testing.T) {
	if _, err := newDiskFileSource("/nonexistent/movie.bin"); err == nil {
		t.Error("expected error opening nonexistent file")
	}
}

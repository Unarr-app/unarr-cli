package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWaitForSegmentCompletedBeforePoll(t *testing.T) {
	for _, tc := range []struct {
		name   string
		data   []byte
		err    error
		wantOK bool
	}{
		{"closed segment", []byte("completed fragment"), nil, true},
		{"empty segment", []byte{}, nil, false},
		{"missing segment", nil, nil, false},
		{"failed encoder", []byte("partial fragment"), errors.New("encode failed"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &HLSSession{tmpDir: t.TempDir(), exited: true, exitErr: tc.err}
			if err := os.MkdirAll(filepath.Join(s.tmpDir, "video"), 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.data != nil {
				if err := os.WriteFile(filepath.Join(s.tmpDir, "video", s.segmentFileName(0)), tc.data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.waitForSegment(context.Background(), 0); (err == nil) != tc.wantOK {
				t.Fatalf("waitForSegment: %v; want success=%t", err, tc.wantOK)
			}
		})
	}
}

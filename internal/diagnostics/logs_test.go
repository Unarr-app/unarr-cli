package diagnostics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogsBoundedAndRotated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unarr.log")
	if err := os.WriteFile(path, []byte("arbitrary secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".1", []byte("panic: user@example.invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s := collectLogFiles("daemon", path)
	if len(s.Events) != 1 || s.Events[0].Event != "panic" || s.OmittedLines != 1 {
		t.Fatalf("rotation missing: %+v", s)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("panic: password\n", 100000)), 0600); err != nil {
		t.Fatal(err)
	}
	s = collectLogFiles("daemon", path)
	if s.ScannedBytes > maxSourceBytes || len(s.Events) != maxSourceEvents || s.Status != "truncated" || s.OmittedLines == 0 {
		t.Fatalf("unbounded source: bytes=%d events=%d status=%s omitted=%d", s.ScannedBytes, len(s.Events), s.Status, s.OmittedLines)
	}
}

func TestLogFilesDoNotFollowSymlinksOrReadDirectories(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "private")
	path := filepath.Join(dir, "unarr.log")
	if err := os.WriteFile(target, []byte("panic: private\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skip("symlinks unavailable")
	}
	for _, p := range []string{path, dir} {
		b, status := readRegularTail(p, 1024)
		if len(b) != 0 || status != "not-regular" {
			t.Fatalf("unsafe read %s: %s", p, status)
		}
	}
}

func TestEveryEventDropsTheEntireOriginalMessage(t *testing.T) {
	secret := "secret@example.invalid 10.4.3.2 /Users/private/file token=secret\x1b[31m"
	for _, marker := range eventMarkers {
		s := LogSource{Events: []Event{}}
		scanEvents(&s, []byte(secret+" "+marker.marker+" "+secret))
		if len(s.Events) != 1 {
			t.Fatalf("missing event for %q", marker.marker)
		}
		b, _ := json.Marshal(s.Events)
		if strings.Contains(string(b), "secret") || strings.Contains(string(b), "10.4.3.2") || strings.Contains(string(b), "Users") {
			t.Fatalf("private message leaked: %s", b)
		}
	}
}

func TestLimitedCommandOutputDiscardsOverflow(t *testing.T) {
	w := &limitedOutput{limit: 5}
	if n, err := w.Write([]byte("123456789")); n != 9 || err != nil {
		t.Fatal(n, err)
	}
	if n, err := w.Write([]byte("secret")); n != 6 || err != nil {
		t.Fatal(n, err)
	}
	if w.String() != "12345" || !w.truncated {
		t.Fatal("unbounded subprocess output")
	}
}

func TestEventTimeIsParsedAndNormalizedWithoutPreservingThePrefix(t *testing.T) {
	for _, tc := range []struct{ line, want string }{
		{"2026-09-18T12:33:11+02:00 panic: private", "2026-09-18T10:33:11Z"},
		{"2026-09-18T10:33:11Z panic: private", "2026-09-18T10:33:11Z"},
		{"2026-09-18T10:33:11.123Z panic: private", "2026-09-18T10:33:11Z"},
		{"john@example.invalid panic: private", ""},
	} {
		s := LogSource{Events: []Event{}}
		scanEvents(&s, []byte(tc.line))
		if len(s.Events) != 1 || s.Events[0].Time != tc.want {
			t.Fatalf("timestamp normalization: %+v", s.Events)
		}
	}
}

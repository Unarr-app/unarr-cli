package engine

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHLSInfoSnapshotsSession(t *testing.T) {
	started := time.Now().Add(-time.Minute)
	s := &HLSSession{cfg: HLSSessionConfig{SessionID: "abc", FileName: "m.mkv", VideoCopy: true}, startedAt: started, lastTouch: started}
	s.speedEWMA, s.speedSamples = 1.3, 4
	s.servedBytes.Add(2048)

	reg := NewHLSSessionRegistry(2)
	reg.sessions["abc"] = s
	infos := reg.Infos()
	if len(infos) != 1 {
		t.Fatalf("infos = %d", len(infos))
	}
	in := infos[0]
	if in.ID != "abc" || in.FileName != "m.mkv" || !in.VideoCopy || in.ServedBytes != 2048 ||
		in.Stats.SpeedX != 1.3 || !in.StartedAt.Equal(started) || in.Failed {
		t.Fatalf("info = %+v", in)
	}
}

func TestCountServedCountsWritesAndKeepsFlush(t *testing.T) {
	s := &HLSSession{}
	rec := httptest.NewRecorder()
	w := s.countServed(rec)

	_, _ = w.Write([]byte("hello"))
	_, _ = w.Write([]byte("!!"))
	if got := s.servedBytes.Load(); got != 7 {
		t.Fatalf("served = %d, want 7", got)
	}
	f, ok := w.(http.Flusher)
	if !ok {
		t.Fatal("wrapper must stay a Flusher (segment handlers stream)")
	}
	f.Flush()
	if !rec.Flushed {
		t.Fatal("Flush not forwarded")
	}
	if http.NewResponseController(w).Flush() != nil {
		t.Fatal("ResponseController cannot reach the real writer (Unwrap)")
	}
}

// readFromWriter is a ResponseWriter whose ReadFrom is the sendfile path.
type readFromWriter struct {
	*httptest.ResponseRecorder
	readFroms int
}

func (w *readFromWriter) ReadFrom(src io.Reader) (int64, error) {
	w.readFroms++
	return io.Copy(w.Body, src)
}

// http.ServeFile copies through io.ReaderFrom (sendfile for an *os.File): the
// counting wrapper must keep that path, and still count what it moves.
func TestCountServedKeepsReaderFrom(t *testing.T) {
	s := &HLSSession{}
	under := &readFromWriter{ResponseRecorder: httptest.NewRecorder()}
	rf, ok := s.countServed(under).(io.ReaderFrom)
	if !ok {
		t.Fatal("wrapper hides io.ReaderFrom: every HLS segment loses sendfile")
	}
	if n, err := rf.ReadFrom(strings.NewReader("segment")); err != nil || n != 7 {
		t.Fatalf("ReadFrom = %d, %v", n, err)
	}
	if under.readFroms != 1 || s.servedBytes.Load() != 7 {
		t.Fatalf("delegated %d time(s), served %d; want 1 / 7", under.readFroms, s.servedBytes.Load())
	}

	// A writer without ReadFrom falls back to plain writes, counted once.
	plain := httptest.NewRecorder()
	rf = s.countServed(plain).(io.ReaderFrom)
	if n, err := rf.ReadFrom(strings.NewReader("abc")); err != nil || n != 3 {
		t.Fatalf("fallback ReadFrom = %d, %v", n, err)
	}
	if plain.Body.String() != "abc" || s.servedBytes.Load() != 10 {
		t.Fatalf("body %q served %d, want abc / 10", plain.Body.String(), s.servedBytes.Load())
	}
}

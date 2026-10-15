package engine

import (
	"net/http"
	"net/http/httptest"
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

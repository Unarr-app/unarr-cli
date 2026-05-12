package engine

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestStreamServerURLsJSON(t *testing.T) {
	ss := &StreamServer{}
	ss.urls = StreamURLs{LAN: "http://10.0.0.1:8000/stream", Tailscale: "http://100.64.0.1:8000/stream"}
	got := ss.URLsJSON()
	if !strings.Contains(got, `"lan":"http://10.0.0.1:8000/stream"`) {
		t.Errorf("URLsJSON missing LAN: %s", got)
	}
	if !strings.Contains(got, `"ts":"http://100.64.0.1:8000/stream"`) {
		t.Errorf("URLsJSON missing Tailscale: %s", got)
	}
}

func TestStreamServerHLSBaseURLs(t *testing.T) {
	ss := &StreamServer{}
	ss.urls = StreamURLs{
		LAN:       "http://10.0.0.1:8000/stream",
		Tailscale: "http://100.64.0.1:8000/stream",
		Public:    "http://1.2.3.4:9000/stream",
	}
	out := ss.hlsBaseURLs("sess-1")
	if out.LAN != "http://10.0.0.1:8000/hls/sess-1" {
		t.Errorf("LAN swap = %q", out.LAN)
	}
	if out.Tailscale != "http://100.64.0.1:8000/hls/sess-1" {
		t.Errorf("Tailscale swap = %q", out.Tailscale)
	}
	if out.Public != "http://1.2.3.4:9000/hls/sess-1" {
		t.Errorf("Public swap = %q", out.Public)
	}

	js := ss.HLSURLsJSON("sess-1")
	if !strings.Contains(js, "/hls/sess-1") {
		t.Errorf("HLSURLsJSON output unexpected: %s", js)
	}
}

func TestStreamServerIdleSinceZeroBeforeActivity(t *testing.T) {
	ss := &StreamServer{}
	if got := ss.IdleSince(); got != 0 {
		t.Errorf("IdleSince before any activity = %v, want 0", got)
	}
	ss.lastActivity.Store(time.Now().Add(-1 * time.Second).UnixNano())
	if got := ss.IdleSince(); got <= 0 {
		t.Errorf("IdleSince after activity should be > 0, got %v", got)
	}
}

func TestDiskFileProvider(t *testing.T) {
	tmp := t.TempDir() + "/movie.mp4"
	data := []byte("hello stream")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewDiskFileProvider(tmp)
	if got := p.FileName(); got != "movie.mp4" {
		t.Errorf("FileName = %q", got)
	}
	if got := p.FileSize(); got != int64(len(data)) {
		t.Errorf("FileSize = %d, want %d", got, len(data))
	}
	rdr := p.NewFileReader(context.Background())
	if rdr == nil {
		t.Fatal("NewFileReader = nil")
	}
	defer rdr.Close()
	buf := make([]byte, len(data))
	n, _ := rdr.Read(buf)
	if string(buf[:n]) != string(data) {
		t.Errorf("read = %q, want %q", buf[:n], data)
	}
}

func TestDiskFileProviderMissing(t *testing.T) {
	p := NewDiskFileProvider("/nonexistent/file.mp4")
	if rdr := p.NewFileReader(context.Background()); rdr != nil {
		t.Errorf("NewFileReader on missing file should return nil")
	}
	if got := p.FileSize(); got != 0 {
		t.Errorf("FileSize on missing file = %d, want 0", got)
	}
}

func TestFindVideoFile(t *testing.T) {
	tmp := t.TempDir()
	os.WriteFile(tmp+"/readme.txt", make([]byte, 1000), 0o644)           //nolint:errcheck
	os.WriteFile(tmp+"/sample.mkv", make([]byte, 10*1024*1024), 0o644)   //nolint:errcheck
	os.WriteFile(tmp+"/clip.mp4", make([]byte, 1024*1024), 0o644)        //nolint:errcheck
	os.MkdirAll(tmp+"/sub", 0o755)                                       //nolint:errcheck
	os.WriteFile(tmp+"/sub/extra.mp4", make([]byte, 5*1024*1024), 0o644) //nolint:errcheck

	got := FindVideoFile(tmp)
	if !strings.HasSuffix(got, "sample.mkv") {
		t.Errorf("FindVideoFile = %q, want largest *.mkv", got)
	}
}

func TestFindVideoFileEmpty(t *testing.T) {
	tmp := t.TempDir()
	if got := FindVideoFile(tmp); got != "" {
		t.Errorf("FindVideoFile on empty dir = %q, want ''", got)
	}
}

func TestLanIPReturnsValidOrEmpty(t *testing.T) {
	ip := LanIP()
	if ip != "" && !strings.Contains(ip, ".") && !strings.Contains(ip, ":") {
		t.Errorf("LanIP returned non-empty non-IP: %q", ip)
	}
}

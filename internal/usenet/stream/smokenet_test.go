//go:build smokenet

// Real-network smoke: streams a real Usenet post through the nntpRangeReader
// against a live NNTP server. Skipped unless SMOKE_NNTP_HOST is set. Run with:
//
//	go test -tags smokenet -run TestSmokeDirectStreamRealNNTP -v ./internal/usenet/stream/
//
// with SMOKE_NNTP_{HOST,PORT,SSL,TLS,USER,PASS}, SMOKE_NZB (path to a .nzb) and
// SMOKE_OUT (where to write the streamed file) exported.
package stream

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntp"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nzb"
)

// TestSmokeStreamOverHTTPRealNNTP is the production-shaped path: ffmpeg (ffprobe)
// reads the nntpRangeReader THROUGH http.ServeContent over HTTP Range — the same
// shape the daemon's /usenet/ endpoint feeds ffmpeg. ffprobe only fetches the
// header + container index (Range requests), so only a few articles are pulled
// from NNTP, not the whole file. Proves the release plays via the real path.
func TestSmokeStreamOverHTTPRealNNTP(t *testing.T) {
	host := os.Getenv("SMOKE_NNTP_HOST")
	if host == "" {
		t.Skip("set SMOKE_NNTP_HOST to run the real-network smoke")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not on PATH")
	}
	port, _ := strconv.Atoi(os.Getenv("SMOKE_NNTP_PORT"))
	if port == 0 {
		port = 563
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client := nntp.NewClient(nntp.Config{
		Host: host, Port: port, SSL: os.Getenv("SMOKE_NNTP_SSL") != "false",
		TLSServerName: os.Getenv("SMOKE_NNTP_TLS"), Username: os.Getenv("SMOKE_NNTP_USER"),
		Password: os.Getenv("SMOKE_NNTP_PASS"), MaxConnections: 8,
	})
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	data, err := os.ReadFile(os.Getenv("SMOKE_NZB"))
	if err != nil {
		t.Fatalf("read nzb: %v", err)
	}
	n, err := nzb.ParseBytes(data)
	if err != nil {
		t.Fatalf("parse nzb: %v", err)
	}
	plan := StreamPlanFromNZB(ctx, client, n)
	if !plan.Streamable() {
		t.Fatalf("not streamable: %s", plan.Reason)
	}

	// Serve the NNTP-backed reader over HTTP exactly like the daemon does: a
	// ReadSeeker fed to http.ServeContent, which turns ffprobe's Range requests
	// into Seeks + Reads on the reader → ranged NNTP fetches.
	r := plan.Open(ctx)
	defer r.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.ServeContent(w, req, plan.VideoName, time.Now(), r)
	}))
	defer srv.Close()

	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error",
		"-show_entries", "format=format_name,duration:stream=codec_type,codec_name",
		"-of", "default=noprint_wrappers=1", srv.URL).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe over HTTP failed: %v\n%s", err, out)
	}
	t.Logf("ffprobe over HTTP (ffmpeg ↔ endpoint ↔ nntpRangeReader ↔ NNTP):\n%s", out)
	if !bytes.Contains(out, []byte("codec_type=video")) {
		t.Fatalf("ffprobe did not see a video stream over the HTTP path:\n%s", out)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func TestSmokeDirectStreamRealNNTP(t *testing.T) {
	host := os.Getenv("SMOKE_NNTP_HOST")
	if host == "" {
		t.Skip("set SMOKE_NNTP_HOST to run the real-network smoke")
	}
	port, _ := strconv.Atoi(os.Getenv("SMOKE_NNTP_PORT"))
	if port == 0 {
		port = 563
	}
	nzbPath := os.Getenv("SMOKE_NZB")
	outPath := os.Getenv("SMOKE_OUT")
	if nzbPath == "" || outPath == "" {
		t.Fatal("SMOKE_NZB and SMOKE_OUT are required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client := nntp.NewClient(nntp.Config{
		Host:           host,
		Port:           port,
		SSL:            os.Getenv("SMOKE_NNTP_SSL") != "false",
		TLSServerName:  os.Getenv("SMOKE_NNTP_TLS"),
		Username:       os.Getenv("SMOKE_NNTP_USER"),
		Password:       os.Getenv("SMOKE_NNTP_PASS"),
		MaxConnections: 8,
	})
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("NNTP connect: %v", err)
	}
	defer client.Close()
	t.Logf("connected to %s:%d", host, port)

	data, err := os.ReadFile(nzbPath)
	if err != nil {
		t.Fatalf("read nzb: %v", err)
	}
	n, err := nzb.ParseBytes(data)
	if err != nil {
		t.Fatalf("parse nzb: %v", err)
	}

	t.Logf("nzb parsed: %d files", len(n.Files))
	for _, f := range n.Files {
		t.Logf("  file=%q segs=%d subject=%q", f.Filename(), len(f.Segments), truncate(f.Subject, 70))
	}
	t.Logf("HasRars=%v ContentFiles=%d", n.HasRars(), len(n.ContentFiles()))

	plan := StreamPlanFromNZB(ctx, client, n)
	if !plan.Streamable() {
		t.Fatalf("release is NOT streamable: %s", plan.Reason)
	}
	t.Logf("plan: kind=%v video=%q size=%d", plan.Kind, plan.VideoName, plan.VideoSize)

	r := plan.Open(ctx)
	defer r.Close()

	// Exercise the container-index seek path (mp4 moov / mkv cues can live at the
	// end): Seek(0,End) for the exact size, then a mid-file seek, then rewind and
	// stream the whole thing — exactly what http.ServeContent + ffmpeg induce.
	sz, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		t.Fatalf("seek end: %v", err)
	}
	t.Logf("seek(0,End) exact size = %d", sz)
	if _, err := r.Seek(sz/2, io.SeekStart); err != nil {
		t.Fatalf("seek middle: %v", err)
	}
	mid := make([]byte, 4096)
	if _, err := io.ReadFull(r, mid); err != nil {
		t.Fatalf("read middle: %v", err)
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("rewind: %v", err)
	}

	out, err := os.Create(outPath)
	if err != nil {
		t.Fatalf("create out: %v", err)
	}
	nbytes, err := io.Copy(out, r)
	closeErr := out.Close()
	if err != nil {
		t.Fatalf("stream copy failed at %d bytes: %v", nbytes, err)
	}
	if closeErr != nil {
		t.Fatalf("close out: %v", closeErr)
	}
	t.Logf("streamed %d bytes to %s (plan size %d)", nbytes, outPath, sz)
	if sz > 0 && nbytes != sz {
		t.Errorf("streamed %d bytes but plan size was %d", nbytes, sz)
	}
}

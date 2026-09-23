package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeFFprobe writes a shell stub standing in for ffprobe.
func fakeFFprobe(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	path := filepath.Join(t.TempDir(), "ffprobe")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil { //nolint:gosec // G306: test stub must be executable.
		t.Fatal(err)
	}
	return path
}

const xtreamURL = "http://panel.example:8080/movie/alice99/s3cr3tPass/4242.mkv"

func assertNoSecrets(t *testing.T, where, text string) {
	t.Helper()
	for _, secret := range []string{"alice99", "s3cr3tPass", xtreamURL} {
		if strings.Contains(text, secret) {
			t.Fatalf("%s leaks %q: %s", where, secret, text)
		}
	}
}

// I3: ffprobe echoes its input on stderr; the start error the daemon logs and
// reports to the web must not carry the account's credentials.
func TestStartHLSSessionRedactsProviderURLInProbeErrors(t *testing.T) {
	// The stub prints its last argument (the input URL), like ffprobe does.
	probe := fakeFFprobe(t, `for a; do last="$a"; done; echo "$last: Server returned 403 Forbidden" >&2; exit 1`)
	_, err := StartHLSSession(context.Background(), HLSSessionConfig{
		SessionID: "redact1",
		SourceURL: xtreamURL,
		Transcode: TranscodeRuntime{FFmpegPath: "/bin/false", FFprobePath: probe},
	})
	if err == nil {
		t.Fatal("expected a probe failure")
	}
	assertNoSecrets(t, "start error", err.Error())
	if !strings.Contains(err.Error(), "panel.example:8080") {
		t.Fatalf("the host must stay (it makes the report actionable): %s", err)
	}
	if !errors.Is(err, ErrSourceUnreachable) {
		t.Fatalf("redaction must keep the classification, got %v", err)
	}
}

func TestRedactSourceTextMasksEveryEcho(t *testing.T) {
	cases := []struct{ raw, text string }{
		{xtreamURL, "Opening '" + xtreamURL + "' for reading"},
		{xtreamURL, "http error on /movie/alice99/s3cr3tPass/4242.mkv"},
		{xtreamURL, "user alice99 rejected"},
		{"https://cdn.example/f.mkv?token=abcdef123456&x=1", "GET https://cdn.example/f.mkv?token=abcdef123456&x=1 -> 410"},
		{"https://cdn.example/f.mkv?token=abcdef123456", "token abcdef123456 expired"},
		{"http://bob:hunter22@host.example/v.mp4", "auth failed for bob:hunter22@host.example"},
	}
	for _, c := range cases {
		got := RedactSourceText(c.text, c.raw)
		for _, secret := range []string{"alice99", "s3cr3tPass", "abcdef123456", "hunter22"} {
			if strings.Contains(got, secret) {
				t.Errorf("RedactSourceText(%q) = %q still has %q", c.text, got, secret)
			}
		}
	}
	if got := RedactSourceText("nothing to see", xtreamURL); got != "nothing to see" {
		t.Errorf("unrelated text changed: %q", got)
	}
	if got := RedactSourceText("x", ""); got != "x" {
		t.Errorf("no URL must be a no-op, got %q", got)
	}
}

// B1: a URL session with no identity must not touch the persistent cache. The
// probe stub reports a video so the start runs past cache placement.
func TestURLSessionWithoutIdentityNeverCaches(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	cacheDir := filepath.Join(t.TempDir(), "cache")
	cache, err := NewHLSCache(cacheDir, 1)
	if err != nil {
		t.Fatal(err)
	}
	probe := fakeFFprobe(t, `echo '{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","width":1280,"height":720}],"format":{"duration":"30.0"}}'`)
	ffmpeg := fakeFFprobe(t, `sleep 5`) // never produces a segment; the test only looks at placement
	for _, u := range []string{"http://p.example/movie/u1/p1/1.mkv", "http://p.example/movie/u1/p1/2.mkv"} {
		s, err := StartHLSSession(context.Background(), HLSSessionConfig{
			SessionID: "nocache" + u[len(u)-5:len(u)-4],
			SourceURL: u,
			Quality:   "720p",
			Transcode: TranscodeRuntime{FFmpegPath: ffmpeg, FFprobePath: probe},
			Cache:     cache,
		})
		if err != nil {
			t.Fatalf("start %s: %v", u, err)
		}
		if s.cacheKey != "" || s.fromCache || s.cache != nil {
			_ = s.Close()
			t.Fatalf("URL session without CacheID was placed in the cache (key=%q hit=%v)", s.cacheKey, s.fromCache)
		}
		_ = s.Close()
	}
	entries, _ := os.ReadDir(cacheDir)
	if len(entries) != 0 {
		t.Fatalf("cache dir got entries: %v", entries)
	}
}

// Different provider URLs keyed through CacheID must land on different keys.
func TestCacheKeyDiffersPerCacheID(t *testing.T) {
	cache, err := NewHLSCache(filepath.Join(t.TempDir(), "cache"), 1)
	if err != nil {
		t.Fatal(err)
	}
	k := func(id string) string {
		return cache.Key(HLSCacheKeyOpts{Source: id, IsID: true, Quality: "720p", AudioIndex: -1, BurnSubtitleIndex: -1})
	}
	if k("url:aaa") == k("url:bbb") {
		t.Fatal("two titles share a cache key")
	}
}

// The source gate is released exactly once on a start failure (never leaks a
// pinned hold) and on Close of a started session.
func TestAcquireSourceReleasedOnFailureAndClose(t *testing.T) {
	var acquired, released atomic.Int32
	gate := func(context.Context) (func(), error) {
		acquired.Add(1)
		return func() { released.Add(1) }, nil
	}
	failing := fakeFFprobe(t, `exit 1`)
	_, err := StartHLSSession(context.Background(), HLSSessionConfig{
		SessionID: "gate1", SourceURL: xtreamURL, AcquireSource: gate,
		Transcode: TranscodeRuntime{FFmpegPath: "/bin/false", FFprobePath: failing},
	})
	if err == nil || acquired.Load() != 1 || released.Load() != 1 {
		t.Fatalf("start failure: err=%v acquired=%d released=%d", err, acquired.Load(), released.Load())
	}

	probe := fakeFFprobe(t, `echo '{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","width":1280,"height":720}],"format":{"duration":"30.0"}}'`)
	ffmpeg := fakeFFprobe(t, `sleep 5`)
	s, err := StartHLSSession(context.Background(), HLSSessionConfig{
		SessionID: "gate2", SourceURL: xtreamURL, AcquireSource: gate, SingleConnection: true,
		Transcode: TranscodeRuntime{FFmpegPath: ffmpeg, FFprobePath: probe},
	})
	if err != nil {
		t.Fatal(err)
	}
	if released.Load() != 1 {
		t.Fatalf("released while the session still reads the source (released=%d)", released.Load())
	}
	_ = s.Close()
	_ = s.Close()
	if released.Load() != 2 {
		t.Fatalf("Close must release exactly once, released=%d", released.Load())
	}
}

// A gate that refuses (the session was superseded or cancelled while waiting)
// fails the start before the probe: the source is never read.
func TestAcquireSourceErrorFailsBeforeAnyRead(t *testing.T) {
	refused := errors.New("superseded")
	probed := fakeFFprobe(t, `touch "$0.ran"; exit 1`)
	_, err := StartHLSSession(context.Background(), HLSSessionConfig{
		SessionID: "gate3", SourceURL: xtreamURL,
		AcquireSource: func(context.Context) (func(), error) { return nil, refused },
		Transcode:     TranscodeRuntime{FFmpegPath: "/bin/false", FFprobePath: probed},
	})
	if !errors.Is(err, refused) {
		t.Fatalf("err=%v, want the gate's error", err)
	}
	if _, statErr := os.Stat(probed + ".ran"); statErr == nil {
		t.Fatal("ffprobe ran although the source gate refused")
	}
}

// A single-connection session takes COPY-VOD (seekable, full manifest) only
// through the source proxy's single upstream link; with the proxy switched off
// its readers would each open a provider connection, so it declines.
func TestSingleConnectionCopyVODNeedsTheSingleUpstreamProxy(t *testing.T) {
	probe := &StreamProbe{VideoCodec: "h264", DurationSec: 60}
	if !copyVODViable(HLSSessionConfig{SingleConnection: true}, probe) {
		t.Fatal("single-connection h264 must be COPY-VOD viable (seek + resume on one connection)")
	}
	t.Setenv(copyVODDirectEnv, "1")
	if copyVODViable(HLSSessionConfig{SingleConnection: true}, probe) {
		t.Fatal("without the source proxy a single-connection source must not be COPY-VOD viable")
	}
	s := &HLSSession{cfg: HLSSessionConfig{SingleConnection: true, SourceURL: xtreamURL, SessionID: "one"}, probe: probe, durationSec: 60}
	if startCopyVOD(context.Background(), s) {
		t.Fatal("startCopyVOD accepted a single-connection source")
	}
	if s.copyProxy != nil || s.copySlots != nil {
		t.Fatal("declined COPY-VOD left a proxy or copy slots behind")
	}
}

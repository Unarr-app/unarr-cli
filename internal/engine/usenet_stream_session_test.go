package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
	"github.com/Unarr-app/unarr-cli/internal/usenet/stream"
)

// hookRecorder captures which transport hook fired (exactly one must, per call)
// so the fallback gate can be asserted deterministically without a daemon.
type hookRecorder struct {
	direct   int
	hls      int
	fallback int
	reason   string
	handle   *UsenetStreamHandle
}

func (h *hookRecorder) hooks() UsenetStreamHooks {
	return UsenetStreamHooks{
		Direct:   func(handle *UsenetStreamHandle) { h.direct++; h.handle = handle },
		HLS:      func(handle *UsenetStreamHandle) { h.hls++; h.handle = handle },
		Fallback: func(reason string) { h.fallback++; h.reason = reason },
	}
}

// streamableHandle builds a real, registered /usenet source (via the fake NNTP
// server) so the dispatch tests exercise a genuine handle, not a stub.
func streamableHandle(t *testing.T, ss *StreamServer, id string, content []byte) *UsenetStreamHandle {
	t.Helper()
	n, articles := nntptest.BuildDirectFile("movie.2024.1080p.mkv", content, 4096)
	client := dialFakeArticles(t, articles)
	handle, err := BuildUsenetStream(context.Background(), client, n, ss, id)
	if err != nil {
		t.Fatalf("BuildUsenetStream: %v", err)
	}
	return handle
}

// TestDispatchDirect: a streamable handle + playMethod "direct" fires ONLY the
// Direct hook and returns UsenetStreamDirect. The registered source stays live
// (the hook owns it) and serves the exact bytes.
func TestDispatchDirect(t *testing.T) {
	content := usenetTestData(20_000)
	ss := NewStreamServer(0, 1)
	handle := streamableHandle(t, ss, "sess-d", content)

	var rec hookRecorder
	mode := dispatchUsenetStream(handle, nil, "direct", "sess-d", rec.hooks())

	if mode != UsenetStreamDirect {
		t.Fatalf("mode = %s, want direct", mode)
	}
	if rec.direct != 1 || rec.hls != 0 || rec.fallback != 0 {
		t.Fatalf("hooks direct=%d hls=%d fallback=%d, want 1/0/0", rec.direct, rec.hls, rec.fallback)
	}
	if ss.ActiveUsenetSources() != 1 {
		t.Fatalf("active sources = %d, want 1 (direct hook keeps it live)", ss.ActiveUsenetSources())
	}
	if body := serveAndReadAll(t, ss, "sess-d"); !bytes.Equal(body, content) {
		t.Fatalf("served %d bytes, want %d", len(body), len(content))
	}
	handle.Close()
}

// TestDispatchHLS: a streamable handle + an empty/non-"direct" playMethod fires
// ONLY the HLS hook (the safe default for a tail-index container) and hands back
// a usable loopback URL.
func TestDispatchHLS(t *testing.T) {
	content := usenetTestData(8_000)
	ss := NewStreamServer(0, 1)
	handle := streamableHandle(t, ss, "sess-h", content)

	var rec hookRecorder
	mode := dispatchUsenetStream(handle, nil, "", "sess-h", rec.hooks())

	if mode != UsenetStreamHLS {
		t.Fatalf("mode = %s, want hls", mode)
	}
	if rec.hls != 1 || rec.direct != 0 || rec.fallback != 0 {
		t.Fatalf("hooks direct=%d hls=%d fallback=%d, want 0/1/0", rec.direct, rec.hls, rec.fallback)
	}
	if rec.handle == nil || rec.handle.LoopbackURL == "" {
		t.Fatal("HLS hook got no usable loopback URL")
	}
	handle.Close()
}

// TestDispatchErrorFallsBack: a TryStreamUsenet error fires ONLY Fallback,
// returns None, and surfaces a clean reason (prefix stripped). Nothing streamed.
func TestDispatchErrorFallsBack(t *testing.T) {
	var rec hookRecorder
	err := fmt.Errorf("usenet stream: %w (compressed rar)", stream.ErrNotStreamable)

	mode := dispatchUsenetStream(nil, err, "direct", "sess-e", rec.hooks())

	if mode != UsenetStreamNone {
		t.Fatalf("mode = %s, want none", mode)
	}
	if rec.fallback != 1 || rec.direct != 0 || rec.hls != 0 {
		t.Fatalf("hooks direct=%d hls=%d fallback=%d, want 0/0/1", rec.direct, rec.hls, rec.fallback)
	}
	if strings.HasPrefix(rec.reason, "usenet stream:") {
		t.Fatalf("reason still carries the internal prefix: %q", rec.reason)
	}
	if !strings.Contains(rec.reason, "compressed rar") {
		t.Fatalf("reason = %q, want it to mention the cause", rec.reason)
	}
}

// TestDispatchNilHandleFallsBack: a nil handle with no error (defensive) falls
// back cleanly instead of panicking.
func TestDispatchNilHandleFallsBack(t *testing.T) {
	var rec hookRecorder
	mode := dispatchUsenetStream(nil, nil, "", "sess-n", rec.hooks())
	if mode != UsenetStreamNone || rec.fallback != 1 {
		t.Fatalf("mode=%s fallback=%d, want none/1", mode, rec.fallback)
	}
}

// TestDispatchMissingDirectHookFallsBack: a streamable direct plan with NO Direct
// hook wired must fall back AND Close the handle so the registered /usenet source
// is not left dangling.
func TestDispatchMissingDirectHookFallsBack(t *testing.T) {
	content := usenetTestData(5_000)
	ss := NewStreamServer(0, 1)
	handle := streamableHandle(t, ss, "sess-nohook", content)
	if ss.ActiveUsenetSources() != 1 {
		t.Fatalf("precondition: active = %d, want 1", ss.ActiveUsenetSources())
	}

	hooks := UsenetStreamHooks{Fallback: func(string) {}} // Direct/HLS nil
	mode := dispatchUsenetStream(handle, nil, "direct", "sess-nohook", hooks)

	if mode != UsenetStreamNone {
		t.Fatalf("mode = %s, want none", mode)
	}
	if ss.ActiveUsenetSources() != 0 {
		t.Fatalf("dangling source: active = %d, want 0 (handle must be Closed)", ss.ActiveUsenetSources())
	}
}

// TestHandleStreamSessionDisabledGate: with the feature OFF the session falls
// straight back WITHOUT resolving an NZB or opening NNTP — proven by a downloader
// with a nil apiClient (any web/NNTP call would panic). This is the opt-in gate.
func TestHandleStreamSessionDisabledGate(t *testing.T) {
	u := NewUsenetDownloader(nil) // nil apiClient: must never be reached
	ss := NewStreamServer(0, 1)
	req := UsenetStreamRequest{SessionID: "sess-off", NzbID: "abc", InfoHash: "hash"}

	var rec hookRecorder
	mode := u.HandleStreamSession(context.Background(), req, ss, false, rec.hooks())

	if mode != UsenetStreamNone {
		t.Fatalf("mode = %s, want none", mode)
	}
	if rec.fallback != 1 || rec.direct != 0 || rec.hls != 0 {
		t.Fatalf("hooks direct=%d hls=%d fallback=%d, want 0/0/1", rec.direct, rec.hls, rec.fallback)
	}
	if !strings.Contains(rec.reason, "disabled") {
		t.Fatalf("reason = %q, want it to say the feature is disabled", rec.reason)
	}
	if ss.ActiveUsenetSources() != 0 {
		t.Fatalf("disabled gate registered %d sources, want 0", ss.ActiveUsenetSources())
	}
}

// TestHandleStreamSessionGuardFallsBack: with the feature ON but a bogus (path-
// traversal-shaped) session id, TryStreamUsenet's guard errors and the session
// falls back cleanly — the fallback gate absorbs setup faults too, not just the
// "not streamable" outcome. nil apiClient again proves the guard runs first.
func TestHandleStreamSessionGuardFallsBack(t *testing.T) {
	u := NewUsenetDownloader(nil)
	ss := NewStreamServer(0, 1)
	req := UsenetStreamRequest{SessionID: "bad/id", NzbID: "abc"}

	var rec hookRecorder
	mode := u.HandleStreamSession(context.Background(), req, ss, true, rec.hooks())

	if mode != UsenetStreamNone || rec.fallback != 1 {
		t.Fatalf("mode=%s fallback=%d, want none/1", mode, rec.fallback)
	}
	if ss.ActiveUsenetSources() != 0 {
		t.Fatalf("guard fallback registered %d sources, want 0", ss.ActiveUsenetSources())
	}
}

func TestUsenetFallbackReason(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, "unknown error"},
		{"strips prefix", errors.New("usenet stream: not streamable (password protected)"), "not streamable (password protected)"},
		{"passthrough", errors.New("connect: dial tcp: refused"), "connect: dial tcp: refused"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := usenetFallbackReason(tc.err); got != tc.want {
				t.Fatalf("usenetFallbackReason = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUsenetStreamModeString(t *testing.T) {
	for mode, want := range map[UsenetStreamMode]string{
		UsenetStreamNone:   "none",
		UsenetStreamDirect: "direct",
		UsenetStreamHLS:    "hls",
	} {
		if got := mode.String(); got != want {
			t.Fatalf("mode %d String() = %q, want %q", mode, got, want)
		}
	}
}

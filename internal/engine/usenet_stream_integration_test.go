package engine

import (
	"bytes"
	"context"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nzb"
	"github.com/Unarr-app/unarr-cli/internal/usenet/stream"
)

// TestUsenetStreamEndToEnd is the consolidated integration test the streaming
// work exists for: it drives a REAL release (posted to the in-memory fake NNTP
// server) through the FULL engine path the daemon uses —
//
//	BuildUsenetStream (plan + register /usenet source)
//	  → dispatchUsenetStream (the daemon's transport dispatcher)
//	    → the Direct / HLS / Fallback hook
//	      → (streamable) http.ServeContent over the real /usenet endpoint
//
// in ONE flow, across the three outcomes that matter for resilience:
//
//   - fichero-directo (.mkv posted straight to yEnc)  → stream OK (Direct hook,
//     exact bytes served over HTTP)
//   - RAR-store (method 0, multi-volume)              → stream OK (HLS hook,
//     video byte-exact across volume borders over HTTP)
//   - RAR comprimido (method != store)                → clean FALLBACK (only the
//     Fallback hook fires, nothing registered, playback never blocked)
//
// It uses no real Usenet account, no ffmpeg, and no daemon: only the fake NNTP
// server + the real StreamServer HTTP handler, so it is fully deterministic.
func TestUsenetStreamEndToEnd(t *testing.T) {
	cases := []struct {
		name string
		// build posts the release to the fake server and returns the parsed NZB,
		// its articles, and the exact video bytes the stream must reproduce.
		build func() (*nzb.NZB, map[string][]byte, []byte)
		// playMethod the web would send: "direct" for a browser-native container
		// (exercises the Direct hook), "" for a tail-index container that ffmpeg
		// remuxes (exercises the HLS loopback hook).
		playMethod   string
		wantMode     UsenetStreamMode
		wantKind     stream.Kind
		streamable   bool
		wantFallback string // substring the fallback reason must contain (streamable=false)
	}{
		{
			name: "direct-file streams",
			build: func() (*nzb.NZB, map[string][]byte, []byte) {
				content := usenetTestData(60_000)
				n, arts := nntptest.BuildDirectFile("movie.2024.1080p.mkv", content, 4096)
				return n, arts, content
			},
			playMethod: "direct",
			wantMode:   UsenetStreamDirect,
			wantKind:   stream.KindDirect,
			streamable: true,
		},
		{
			name: "rar-store streams",
			build: func() (*nzb.NZB, map[string][]byte, []byte) {
				content := usenetTestData(30_000)
				n, arts := nntptest.BuildRarStore("show.s01e01.mkv", content, 8000, 1200)
				return n, arts, content
			},
			playMethod: "", // tail-index container → HLS loopback for ffmpeg
			wantMode:   UsenetStreamHLS,
			wantKind:   stream.KindRarStore,
			streamable: true,
		},
		{
			name: "compressed-rar falls back",
			build: func() (*nzb.NZB, map[string][]byte, []byte) {
				content := usenetTestData(12_000)
				n, arts := nntptest.BuildRarCompressed("movie.mkv", content, 5000, 1000)
				return n, arts, content
			},
			playMethod:   "direct",
			wantMode:     UsenetStreamNone,
			streamable:   false,
			wantFallback: "not streamable",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, articles, content := tc.build()
			client := dialFakeArticles(t, articles)
			ss := NewStreamServer(0, 1)
			id := "sess-e2e"

			// Plan + register (or, for the compressed release, return the
			// ErrNotStreamable sentinel with nothing registered).
			handle, buildErr := BuildUsenetStream(context.Background(), client, n, ss, id)

			var rec hookRecorder
			mode := dispatchUsenetStream(handle, buildErr, tc.playMethod, id, rec.hooks())

			if mode != tc.wantMode {
				t.Fatalf("mode = %s, want %s", mode, tc.wantMode)
			}

			if !tc.streamable {
				// Resilient floor: ONLY Fallback fires, with a clear reason, and no
				// /usenet source is left registered — the daemon then runs the
				// intact batch download and playback is never broken.
				if rec.fallback != 1 || rec.direct != 0 || rec.hls != 0 {
					t.Fatalf("hooks direct=%d hls=%d fallback=%d, want 0/0/1", rec.direct, rec.hls, rec.fallback)
				}
				if !bytes.Contains([]byte(rec.reason), []byte(tc.wantFallback)) {
					t.Fatalf("fallback reason = %q, want it to contain %q", rec.reason, tc.wantFallback)
				}
				if ss.ActiveUsenetSources() != 0 {
					t.Fatalf("non-streamable release registered %d sources, want 0", ss.ActiveUsenetSources())
				}
				return
			}

			// Streamable: exactly the expected transport hook fired, the handle
			// carries the right kind, and the source stays live (the hook owns it).
			if buildErr != nil || handle == nil {
				t.Fatalf("streamable build failed: err=%v handle=%v", buildErr, handle)
			}
			if handle.Kind != tc.wantKind {
				t.Fatalf("Kind = %s, want %s", handle.Kind, tc.wantKind)
			}
			gotDirect, gotHLS := rec.direct == 1, rec.hls == 1
			wantDirect := tc.wantMode == UsenetStreamDirect
			if rec.fallback != 0 || gotDirect != wantDirect || gotHLS == wantDirect {
				t.Fatalf("hooks direct=%d hls=%d fallback=%d for mode %s", rec.direct, rec.hls, rec.fallback, tc.wantMode)
			}
			if ss.ActiveUsenetSources() != 1 {
				t.Fatalf("streamable source count = %d, want 1", ss.ActiveUsenetSources())
			}

			// The whole point: the registered source reproduces the exact video
			// bytes over the real /usenet HTTP endpoint — the same ranged read
			// ffmpeg (HLS) or the browser (direct) performs. For rar-store this
			// crosses volume borders; for direct it walks the yEnc parts.
			if body := serveAndReadAll(t, ss, id); !bytes.Equal(body, content) {
				t.Fatalf("served %d bytes over HTTP, want %d (byte-exact)", len(body), len(content))
			}

			handle.Close()
			if ss.ActiveUsenetSources() != 0 {
				t.Fatalf("after Close: %d sources, want 0", ss.ActiveUsenetSources())
			}
		})
	}
}

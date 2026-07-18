// Package engine — usenet_stream_session.go is the RESILIENT bridge between the
// daemon's stream-session decision tree and the on-the-fly Usenet streamer
// (usenet_stream.go / internal/usenet/stream). It answers one question for a
// stream session: "can this Usenet release play NOW, straight from NNTP, or must
// we fall back to the batch download?" — and NEVER lets the answer break
// playback.
//
// The whole point is opt-in optimisation with a clean floor:
//   - feature OFF (downloads.usenet_streaming=false)          → batch download
//   - not streamable (compressed/encrypted RAR, password, …)  → batch download
//   - NZB fetch/parse/NNTP setup fault                         → batch download
//   - streamable                                              → serve live (direct
//     play over /stream, or an HLS/remux loopback URL for a tail-index container)
//
// Every non-stream outcome routes through hooks.Fallback with a human-readable
// reason (logged), so the caller runs the intact UsenetDownloader.Download. The
// transport wiring lives in injected hooks (Direct / HLS / Fallback) so this
// decision + the fallback gate are unit-testable against a fake NNTP server with
// no daemon, no ffmpeg, and no real Usenet account.
package engine

import (
	"context"
	"log"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// UsenetStreamMode is how a Usenet stream session was resolved. Exactly one is
// returned per HandleStreamSession call.
type UsenetStreamMode int

const (
	// UsenetStreamNone: the release was NOT streamed — hooks.Fallback fired and
	// the caller MUST run the batch download (feature off, not streamable, or a
	// setup fault). This is the resilient default, never a playback failure.
	UsenetStreamNone UsenetStreamMode = iota
	// UsenetStreamDirect: a /usenet source is registered and served for direct
	// play over /stream (HTTP Range, no ffmpeg) — a browser-native container.
	UsenetStreamDirect
	// UsenetStreamHLS: a /usenet source is registered; the caller drives ffmpeg
	// with the handle's LoopbackURL — for a container whose index sits at the tail
	// (mkv Cues, non-faststart mp4) or one that needs transcode/copy.
	UsenetStreamHLS
)

// String renders the mode for logs.
func (m UsenetStreamMode) String() string {
	switch m {
	case UsenetStreamDirect:
		return "direct"
	case UsenetStreamHLS:
		return "hls"
	default:
		return "none"
	}
}

// UsenetStreamRequest is the daemon-agnostic description of a Usenet stream
// session — just enough to resolve + plan the NZB. Everything the transport
// needs (provider, byte-exact size, loopback URL) comes back on the handle.
type UsenetStreamRequest struct {
	SessionID   string // opaque session id; also the /usenet registry key
	NzbID       string // pre-resolved NZB id (empty → resolve from InfoHash/Title)
	NzbPassword string // set → encrypted → never streamable (clean fallback)
	InfoHash    string // release hash; used as the HLS cache key
	Title       string // release title, for NZB resolution + the served name
	// PlayMethod mirrors StreamSession.PlayMethod: "direct" serves the provider
	// over /stream with no ffmpeg; anything else (incl. "") takes the HLS/remux
	// path, the universally-safe default for a tail-index container. The web picks
	// this from the release container metadata.
	PlayMethod string
}

// UsenetStreamHooks are the transport callbacks HandleStreamSession drives.
// Exactly ONE fires per call: Direct or HLS for a streamable release, Fallback
// otherwise. Keeping the wiring behind injected hooks is what makes the
// streamability decision + fallback gate testable without the daemon.
type UsenetStreamHooks struct {
	// Direct wires handle.Provider onto /stream for direct play and reports ready.
	// It OWNS the handle from here (must arrange handle.Close on session end).
	Direct func(handle *UsenetStreamHandle)
	// HLS hands handle.LoopbackURL to ffmpeg as an HLS/remux SourceURL. It OWNS
	// the handle from here (must arrange handle.Close on session end).
	HLS func(handle *UsenetStreamHandle)
	// Fallback runs the intact batch download. reason is a human-readable,
	// log-safe explanation (e.g. "not streamable (compressed rar)").
	Fallback func(reason string)
}

// HandleStreamSession is the resilient entry point for a Usenet stream session.
// It NEVER downloads the whole release and NEVER fails playback outright: on any
// obstacle it fires hooks.Fallback (returning UsenetStreamNone) so the caller
// runs the batch download. On a streamable release it registers a /usenet source
// on srv and fires Direct or HLS per req.PlayMethod, returning the matching mode.
//
// enabled is the downloads.usenet_streaming gate: when false the session falls
// straight back to the batch download — the feature is strictly opt-in.
func (u *UsenetDownloader) HandleStreamSession(ctx context.Context, req UsenetStreamRequest, srv *StreamServer, enabled bool, hooks UsenetStreamHooks) UsenetStreamMode {
	// Gate first, BEFORE touching the web API / NNTP: an opted-out daemon must
	// fall back without resolving an NZB or opening a connection.
	if !enabled {
		return fireUsenetFallback(hooks, req.SessionID, "usenet streaming disabled (downloads.usenet_streaming=false)")
	}

	task := &Task{
		ID:          req.SessionID,
		InfoHash:    req.InfoHash,
		Title:       req.Title,
		NzbID:       req.NzbID,
		NzbPassword: req.NzbPassword,
	}

	// TryStreamUsenet resolves + plans the NZB and registers a /usenet source when
	// streamable; on any error it registered nothing, so no source leaks below.
	handle, err := u.TryStreamUsenet(ctx, task, srv, req.SessionID)
	return dispatchUsenetStream(handle, err, req.PlayMethod, req.SessionID, hooks)
}

// dispatchUsenetStream turns a TryStreamUsenet/BuildUsenetStream outcome
// (handle-or-error) into the right transport hook + mode. It is PURE — no NNTP,
// no web API — so the fallback gate is unit-testable with a fabricated handle or
// error: an err (or nil handle) fires Fallback; a streamable handle fires Direct
// (browser-native container) or HLS (everything else, incl. tail-index mkv). A
// missing hook is itself a clean fallback, and the handle is Closed so a
// registered /usenet source is never left dangling.
func dispatchUsenetStream(handle *UsenetStreamHandle, err error, playMethod, sessionID string, hooks UsenetStreamHooks) UsenetStreamMode {
	if err != nil {
		return fireUsenetFallback(hooks, sessionID, usenetFallbackReason(err))
	}
	if handle == nil {
		return fireUsenetFallback(hooks, sessionID, "nil stream handle")
	}

	sid := agent.ShortID(sessionID)
	// playMethod == "direct" only when the web is sure the container is
	// browser-native; every other value (incl. "") uses the HLS/remux loopback,
	// which safely handles a tail-index container via ffmpeg's cheap NNTP seeks.
	if playMethod == "direct" {
		if hooks.Direct == nil {
			handle.Close()
			return fireUsenetFallback(hooks, sessionID, "no direct-play hook wired")
		}
		log.Printf("[usenet-stream %s] direct-play %s (%d bytes)", sid, handle.VideoName, handle.VideoSize)
		hooks.Direct(handle)
		return UsenetStreamDirect
	}

	if hooks.HLS == nil {
		handle.Close()
		return fireUsenetFallback(hooks, sessionID, "no hls hook wired")
	}
	log.Printf("[usenet-stream %s] hls loopback %s (%d bytes)", sid, handle.VideoName, handle.VideoSize)
	hooks.HLS(handle)
	return UsenetStreamHLS
}

// fireUsenetFallback logs the reason (every fallback decision stays visible) and
// invokes hooks.Fallback, returning UsenetStreamNone so callers can `return
// fireUsenetFallback(...)`.
func fireUsenetFallback(hooks UsenetStreamHooks, sessionID, reason string) UsenetStreamMode {
	log.Printf("[usenet-stream %s] falling back to download: %s", agent.ShortID(sessionID), reason)
	if hooks.Fallback != nil {
		hooks.Fallback(reason)
	}
	return UsenetStreamNone
}

// usenetFallbackReason turns a TryStreamUsenet error into a short, human-readable
// reason for the fallback log and the user-facing "downloading instead" message.
// It strips the internal "usenet stream: " prefix so a not-streamable error reads
// as e.g. "not streamable (compressed rar)". A nil error is defensive only.
func usenetFallbackReason(err error) string {
	if err == nil {
		return "unknown error"
	}
	return strings.TrimPrefix(err.Error(), "usenet stream: ")
}

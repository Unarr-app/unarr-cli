// Package engine — usenet_stream.go is the orchestrator that turns a resolved
// NZB into a live, on-the-fly Usenet stream: it builds the streamability plan
// (internal/usenet/stream), and when the release is streamable it registers a
// /usenet source on the StreamServer and hands back a loopback URL that ffmpeg /
// HLS consume as a SourceURL. It is a pure OPTIMISATION layer over the batch
// UsenetDownloader.Download pipeline — it never downloads the whole release and
// never mutates the batch path. Any obstacle (compressed/encrypted RAR,
// password, ambiguous video, NZB/NNTP failure) yields a clear fallback signal so
// the caller runs the intact batch download instead.
//
// The design is split into a pure core and a thin API shell so the streaming
// decision is deterministically testable against a fake NNTP server:
//   - BuildUsenetStream: pure core — given an already-parsed *nzb.NZB and an
//     ArticleFetcher, it plans + registers + returns the handle. No web API.
//   - TryStreamUsenet: the daemon-facing shell — resolves + fetches + parses the
//     NZB via the web API (reusing the batch downloader's helpers) and the shared
//     NNTP connection pool, then delegates to BuildUsenetStream.
package engine

import (
	"context"
	"fmt"
	"log"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nzb"
	"github.com/Unarr-app/unarr-cli/internal/usenet/stream"
)

// UsenetStreamHandle is the outcome of a successful stream setup: a live
// /usenet source (already registered on the StreamServer under SourceID) plus
// everything the caller needs to serve it across Muro 2 (index-at-tail
// containers). Two consumption paths, the caller picks per container/codec:
//   - Provider: serve a browser-native container directly over /stream via
//     StreamServer.SetFile — zero ffmpeg, instant seek (mp4 h264/aac faststart).
//   - LoopbackURL: hand to ffmpeg as an HLS/remux SourceURL for a container
//     whose index sits at the tail (mkv Cues, non-faststart mp4) or that needs
//     transcoding; the NNTP reader's cheap random-access Seek reads the tail.
//
// Close unregisters the source; call it when the session ends. The shared NNTP
// connection pool is owned by the UsenetDownloader and is NEVER torn down here.
type UsenetStreamHandle struct {
	Kind        stream.Kind
	VideoName   string
	VideoSize   int64
	Provider    FileProvider
	LoopbackURL string
	SourceID    string

	srv *StreamServer
}

// Close unregisters the /usenet source from the StreamServer. Idempotent and
// safe on a nil handle, so a caller can `defer handle.Close()` unconditionally.
func (h *UsenetStreamHandle) Close() {
	if h == nil || h.srv == nil {
		return
	}
	h.srv.UnregisterUsenetSource(h.SourceID)
	h.srv = nil
}

// BuildUsenetStream is the pure core: given a parsed NZB and an article fetcher,
// it decides whether the release is streamable and, if so, registers a /usenet
// source on srv under sourceID and returns a ready handle (provider + loopback
// URL). It performs NO web-API calls and NO whole-file download — at most it
// reads a few articles (RAR headers / the tail article to pin the exact size).
//
// Fallback is explicit — the caller MUST run the batch download on any error:
//   - stream.ErrNotStreamable (via errors.Is): a normal, logged "can't stream
//     this release" outcome (compressed/encrypted RAR, password, ambiguous/no
//     video, unreadable header). Nothing is registered.
//   - any other error: a hard setup fault (nil srv, invalid sourceID). Nothing
//     is registered.
func BuildUsenetStream(ctx context.Context, fetcher stream.ArticleFetcher, n *nzb.NZB, srv *StreamServer, sourceID string) (*UsenetStreamHandle, error) {
	if srv == nil {
		return nil, fmt.Errorf("usenet stream: nil stream server")
	}
	if !validSessionID.MatchString(sourceID) {
		return nil, fmt.Errorf("usenet stream: invalid source id %q", sourceID)
	}

	plan := stream.StreamPlanFromNZB(ctx, fetcher, n)
	if !plan.Streamable() {
		// A normal outcome — StreamPlanFromNZB already logged the reason. Wrap the
		// sentinel so errors.Is(err, stream.ErrNotStreamable) drives the caller's
		// fallback while the reason text stays in the message for the log.
		return nil, fmt.Errorf("usenet stream: %w (%s)", stream.ErrNotStreamable, plan.Reason)
	}

	provider := NewUsenetFileProvider(plan.VideoName, plan.VideoSize, plan.Open)
	if provider == nil {
		// Defensive: a streamable plan always yields an opener. Treat a nil
		// provider as non-streamable rather than register a dead source that would
		// 500 the endpoint on first read.
		log.Printf("[usenet-stream] streamable plan for %q produced a nil provider — falling back to batch", plan.VideoName)
		return nil, fmt.Errorf("usenet stream: %w (nil provider for %q)", stream.ErrNotStreamable, plan.VideoName)
	}

	srv.RegisterUsenetSource(sourceID, provider)
	handle := &UsenetStreamHandle{
		Kind:        plan.Kind,
		VideoName:   plan.VideoName,
		VideoSize:   plan.VideoSize,
		Provider:    provider,
		LoopbackURL: srv.UsenetLoopbackURL(sourceID),
		SourceID:    sourceID,
		srv:         srv,
	}
	log.Printf("[usenet-stream] %s ready: %s (%d bytes) source=%s", plan.Kind, plan.VideoName, plan.VideoSize, sourceID)
	return handle, nil
}

// TryStreamUsenet is the daemon-facing entry point: it resolves and parses the
// task's NZB via the web API (reusing the batch downloader's resolveNzbID +
// DownloadNzb + ParseBytes), obtains the shared NNTP connection pool (same one
// the batch download uses), then delegates to BuildUsenetStream. srv is the live
// StreamServer; sourceID is the opaque id to register under (the daemon passes
// its session id — already validSessionID-shaped).
//
// It NEVER downloads the whole release and NEVER touches the batch pipeline, so
// a fallback (any returned error — check errors.Is(err, stream.ErrNotStreamable)
// for the "not streamable" case) leaves UsenetDownloader.Download able to run
// the release cleanly from scratch.
func (u *UsenetDownloader) TryStreamUsenet(ctx context.Context, task *Task, srv *StreamServer, sourceID string) (*UsenetStreamHandle, error) {
	// Cheap, API-free guards first so a caller can rely on these without a live
	// web API / NNTP server (and so tests exercise them in isolation).
	if srv == nil {
		return nil, fmt.Errorf("usenet stream: nil stream server")
	}
	if !validSessionID.MatchString(sourceID) {
		return nil, fmt.Errorf("usenet stream: invalid source id %q", sourceID)
	}

	nzbFile, err := u.loadNZBForStream(ctx, task)
	if err != nil {
		return nil, err
	}

	fetcher, err := u.streamFetcher(ctx)
	if err != nil {
		return nil, fmt.Errorf("usenet stream: nntp: %w", err)
	}

	return BuildUsenetStream(ctx, fetcher, nzbFile, srv, sourceID)
}

// loadNZBForStream resolves and parses the task's NZB WITHOUT the batch path's
// resume-cache side effects: it reuses resolveNzbID (the shared resolver) then
// fetches + parses the bytes in memory. It writes nothing to disk and mutates no
// download state, so a streamable-check that falls through to Download finds the
// batch pipeline exactly as it was.
func (u *UsenetDownloader) loadNZBForStream(ctx context.Context, task *Task) (*nzb.NZB, error) {
	nzbID, _, err := u.resolveNzbID(ctx, task)
	if err != nil {
		return nil, fmt.Errorf("usenet stream: resolve nzb: %w", err)
	}
	data, err := u.apiClient.DownloadNzb(ctx, nzbID)
	if err != nil {
		return nil, fmt.Errorf("usenet stream: download nzb %s: %w", nzbID, err)
	}
	parsed, err := nzb.ParseBytes(data)
	if err != nil {
		return nil, fmt.Errorf("usenet stream: parse nzb %s: %w", nzbID, err)
	}
	return parsed, nil
}

// streamFetcher returns the shared NNTP client (as a stream.ArticleFetcher),
// creating/connecting it via the same cached path the batch download uses so a
// stream and a batch download share one connection pool. The client stays owned
// by u — the handle's Close never tears it down.
func (u *UsenetDownloader) streamFetcher(ctx context.Context) (stream.ArticleFetcher, error) {
	creds, err := u.getCredentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("credentials: %w", err)
	}
	client, err := u.getOrCreateNNTP(ctx, creds)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return client, nil
}

package stream

import (
	"context"
	"errors"
	"io"
	"log"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nzb"
)

// Kind classifies how (or whether) an NZB release can be streamed on the fly.
// KindUnsupported is the zero value, so a zero StreamPlan is "not streamable" —
// the safe default when in any doubt.
type Kind int

const (
	// KindUnsupported: the release cannot be streamed (compressed/encrypted RAR,
	// password, ambiguous or missing video). The caller MUST fall back to the
	// batch download; this is a normal, logged outcome, never a failure.
	KindUnsupported Kind = iota
	// KindDirect: a plain video file posted directly in the NZB (no RAR). Its
	// articles reassemble verbatim into the .mkv/.mp4, streamed by a Reader.
	KindDirect
	// KindRarStore: a plain STORE (0% compression) RAR archive holding exactly one
	// video, streamed by locating the video inside the volumes without extracting.
	KindRarStore
)

// String renders the kind for logs.
func (k Kind) String() string {
	switch k {
	case KindDirect:
		return "direct"
	case KindRarStore:
		return "rar-store"
	default:
		return "unsupported"
	}
}

// StreamPlan is the decision produced by StreamPlanFromNZB: whether a release is
// streamable and, if so, how to open a fresh reader over its video. When Kind is
// KindUnsupported, Reason explains why (for the log) and Open returns nil.
// Otherwise VideoName/VideoSize describe the video and Open builds a fresh
// single-consumer io.ReadSeekCloser (as http.ServeContent / ffmpeg expect),
// mirroring the debrid provider so each request gets its own reader.
type StreamPlan struct {
	Kind      Kind
	VideoName string
	VideoSize int64  // exact byte length of the video
	Reason    string // human-readable cause, set only when KindUnsupported

	open func(ctx context.Context) io.ReadSeekCloser
}

// Streamable reports whether the plan yields a playable stream. Callers gate on
// this before wiring the StreamServer; a false plan means "download via batch".
func (p *StreamPlan) Streamable() bool {
	return p.Kind != KindUnsupported && p.open != nil
}

// Open builds a fresh io.ReadSeekCloser over the video, or nil for an
// unsupported plan. Each call opens its own reader, so the plan can back repeated
// (ranged) HTTP requests just like the debrid file provider.
func (p *StreamPlan) Open(ctx context.Context) io.ReadSeekCloser {
	if p.open == nil {
		return nil
	}
	return p.open(ctx)
}

// StreamPlanFromNZB decides whether n can be streamed and returns a StreamPlan.
// It is conservative by construction: a password, a compressed or encrypted RAR,
// an ambiguous multi-video release, or an unreachable/undecodable first article
// all yield KindUnsupported so the caller falls back cleanly to the batch
// download. It never downloads a whole file — at most it reads RAR headers (for
// the RAR path) or one article (to pin the exact size, for the direct path).
func StreamPlanFromNZB(ctx context.Context, fetcher ArticleFetcher, n *nzb.NZB) *StreamPlan {
	if n == nil || len(n.Files) == 0 {
		return unsupportedPlan("empty nzb")
	}
	if n.Password != "" {
		return unsupportedPlan("password protected")
	}
	if n.HasRars() {
		return planRarStore(ctx, fetcher, n)
	}
	return planDirect(ctx, fetcher, n)
}

// planRarStore probes the RAR volumes' headers and builds a rar-store plan, or an
// unsupported plan carrying the probe's reason (compressed, encrypted, no/ambiguous
// video, unreadable header).
func planRarStore(ctx context.Context, fetcher ArticleFetcher, n *nzb.NZB) *StreamPlan {
	rs, err := Probe(ctx, fetcher, n.RarFiles())
	if err != nil {
		return unsupportedPlan(streamableReason(err))
	}
	log.Printf("[usenet-stream] plan rar-store: %s (%d bytes)", rs.VideoName(), rs.VideoSize())
	return &StreamPlan{
		Kind:      KindRarStore,
		VideoName: rs.VideoName(),
		VideoSize: rs.VideoSize(),
		open:      rs.OpenVideo,
	}
}

// planDirect selects the single directly-posted video file and builds a direct
// plan. It establishes the exact size up front (one article), which both gives
// http.ServeContent an accurate length and validates that the first article is
// actually fetchable/decodable — if not, it degrades to unsupported so the batch
// path takes over.
func planDirect(ctx context.Context, fetcher ArticleFetcher, n *nzb.NZB) *StreamPlan {
	video, err := selectDirectVideo(n)
	if err != nil {
		return unsupportedPlan(streamableReason(err))
	}
	size, err := establishSize(ctx, fetcher, *video)
	if err != nil {
		return unsupportedPlan("establish size " + video.Filename() + ": " + err.Error())
	}
	f := *video
	name := f.Filename()
	open := func(c context.Context) io.ReadSeekCloser {
		return NewReader(c, fetcher, f, NewOffsetIndex(f))
	}
	log.Printf("[usenet-stream] plan direct: %s (%d bytes)", name, size)
	return &StreamPlan{Kind: KindDirect, VideoName: name, VideoSize: size, open: open}
}

// selectDirectVideo returns the one streamable video file among the NZB's content
// files (par2/nfo/sfv/sample already excluded by ContentFiles). No video, or more
// than one video, is not streamable (ambiguous) — fall back to batch.
func selectDirectVideo(n *nzb.NZB) (*nzb.File, error) {
	var videos []nzb.File
	for _, f := range n.ContentFiles() {
		if isVideoName(f.Filename()) {
			videos = append(videos, f)
		}
	}
	if len(videos) == 0 {
		return nil, notStreamable("no video file")
	}
	if len(videos) > 1 {
		return nil, notStreamable("multiple video files (ambiguous)")
	}
	v := videos[0]
	return &v, nil
}

// establishSize opens a throwaway Reader over f and Seeks to the end, which
// fetches and observes exactly one article to pin the byte-exact file size. The
// reader is closed immediately; production streams open their own fresh readers.
func establishSize(ctx context.Context, fetcher ArticleFetcher, f nzb.File) (int64, error) {
	r := NewReader(ctx, fetcher, f, NewOffsetIndex(f))
	defer func() { _ = r.Close() }()
	return r.Seek(0, io.SeekEnd)
}

// streamableReason extracts a NotStreamableError's human reason, falling back to
// the raw error text for any other failure (e.g. a transport error while reading
// headers) so the log always explains the fallback.
func streamableReason(err error) string {
	var nse *NotStreamableError
	if errors.As(err, &nse) {
		return nse.Reason
	}
	return err.Error()
}

// unsupportedPlan logs the reason and returns a non-streamable plan. Logging here
// keeps every fallback decision visible (no silent drops).
func unsupportedPlan(reason string) *StreamPlan {
	log.Printf("[usenet-stream] not streamable: %s", reason)
	return &StreamPlan{Kind: KindUnsupported, Reason: reason}
}

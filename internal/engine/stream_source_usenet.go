// Package engine — stream_source_usenet.go implements a FileProvider that serves
// a /usenet/<id> session straight from a Usenet stream reader (hueco #3 for
// NNTP). No local file is involved: the caller's Range requests are translated
// into on-the-fly NNTP article fetches by the injected opener, so a streamable
// release plays in seconds without ever downloading the whole file to disk.
//
// The opener is produced by the streamability plan
// (internal/usenet/stream.StreamPlan.Open / RarStore.OpenVideo). This provider
// stays a pure pass-through — the same role diskFileProvider / debridFileProvider
// play, just backed by an NNTP-range io.ReadSeekCloser instead of a file handle
// or an HTTP link. http.ServeContent drives it exactly like a local file: it
// Seeks to discover size + the range start (cheap, random-access), then Reads
// (lazy article fetch).
package engine

import (
	"context"
	"io"
	"sync"
)

// UsenetOpener builds a fresh single-consumer io.ReadSeekCloser over a
// streamable release's video. Each /usenet request opens its own reader (just
// like NewDebridFileProvider hands out a fresh ranged reader per connection), so
// concurrent ranged reads from ffmpeg never share cursor state. The returned
// reader must own the passed context (cancelled when the connection ends).
type UsenetOpener func(ctx context.Context) io.ReadSeekCloser

// usenetFileProvider serves a Usenet stream via an opener func. It carries the
// video's display name (drives the served Content-Type) and its byte-exact size
// (needed by http.ServeContent to range-serve) resolved up front by the plan.
type usenetFileProvider struct {
	name string
	size int64
	open UsenetOpener

	// release frees the per-source state behind open (the plan's shared decoded
	// article cache). nil when there is none. Run at most once.
	release     func()
	releaseOnce sync.Once
}

// releasableSource is a FileProvider holding per-source state that must be freed
// when the source is unregistered.
type releasableSource interface{ Release() }

// NewUsenetFileProvider builds a FileProvider backed by a Usenet stream opener.
// name is the video's file name (with extension, so mimeTypeFromExt yields the
// right Content-Type), size is its byte-exact length (from the plan), and open
// mints a fresh reader per call. Returns nil when open is nil — a provider with
// no way to produce bytes is never useful, and a nil provider makes the caller's
// SetFile/Register a clear no-op rather than a handler that panics on first read.
func NewUsenetFileProvider(name string, size int64, open UsenetOpener) FileProvider {
	return newReleasableUsenetProvider(name, size, open, nil)
}

// newReleasableUsenetProvider is NewUsenetFileProvider plus a release hook run
// (once) when the source is unregistered from the StreamServer.
func newReleasableUsenetProvider(name string, size int64, open UsenetOpener, release func()) FileProvider {
	if open == nil {
		return nil
	}
	return &usenetFileProvider{name: name, size: size, open: open, release: release}
}

// Release frees the provider's per-source state. Idempotent. Readers already
// open keep serving correct bytes.
func (p *usenetFileProvider) Release() {
	if p.release != nil {
		p.releaseOnce.Do(p.release)
	}
}

func (p *usenetFileProvider) NewFileReader(ctx context.Context) io.ReadSeekCloser {
	return p.open(ctx)
}

func (p *usenetFileProvider) FileName() string { return p.name }
func (p *usenetFileProvider) FileSize() int64  { return p.size }

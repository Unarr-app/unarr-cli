// Package srcproxy puts a loopback, range-caching HTTP proxy in front of a
// remote media source (debrid CDN, usenet bridge) for COPY-VOD playback.
//
// COPY-VOD spawns one short ffmpeg per segment, and each of them re-reads the
// container header (megabytes when fonts are attached) and the seek index over
// the network before touching the few seconds it actually needs — measured at
// ~26 MB fetched per 8 MB segment, in 5-6 sequential round trips. The proxy keeps
// the header/index pinned and the most recent data blocks in an LRU, so a segment
// costs one upstream request for (nearly) only its own bytes.
//
// It also arbitrates the link: a Background client (the whole-file subtitle
// extractor) only receives bytes while no Foreground client (a segment) is
// reading, so subtitles can never starve the picture.
package srcproxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	blockSize         = 256 << 10
	defaultCacheBytes = 128 << 20
	defaultPinHead    = 16 << 20
	defaultPinTail    = 4 << 20
	refreshTimeout    = 20 * time.Second
	// backgroundDrip is the longest a Background reader is held back in one go.
	// A trickle keeps its upstream connection and ffmpeg's -rw_timeout alive.
	backgroundDrip = 5 * time.Second
	// defaultLinkStall is how long the single-upstream link waits without a
	// byte before giving up on an operation (headers or block) and releasing
	// the link. Well above a slow panel's gaps, well below "wedged forever".
	defaultLinkStall = 20 * time.Second
	// departedStall replaces it once the client that asked for the block has
	// gone away: finish a block that is still flowing (the next reader may
	// reuse it), but don't hold everyone else behind one that is not.
	departedStall = 2 * time.Second
	// Same UA the seek-index reader uses; some debrid CDNs reject Go's default.
	userAgent = "VLC/3.0.20 LibVLC/3.0.20"
)

// Options configures a Proxy. Only URL and Dir are required.
type Options struct {
	URL string
	// Refresh re-resolves an expired signed link. Optional.
	Refresh func(context.Context) (string, error)
	// Dir holds the cache slot file; removed on Close.
	Dir        string
	CacheBytes int64
	PinHead    int64
	PinTail    int64
	Client     *http.Client
	// SingleUpstream funnels every client through ONE upstream reader, so the
	// source never sees two connections at once however many local readers
	// (segment spawns, index, subtitles) the proxy serves — for a provider that
	// allows a single connection per account (IPTV). Reads are serialized block
	// by block; a reader continuing where the last one stopped reuses the open
	// response, a jump elsewhere closes it before opening the next.
	SingleUpstream bool
	// StallTimeout bounds how long the single-upstream link waits with no byte
	// arriving before it abandons the operation (default defaultLinkStall).
	StallTimeout time.Duration
}

// Stats is a point-in-time snapshot of proxy traffic.
type Stats struct {
	UpstreamRequests int64
	UpstreamBytes    int64
	ServedBytes      int64
	CacheHitBytes    int64
}

type counters struct {
	upstreamRequests, upstreamBytes, servedBytes, cacheHitBytes atomic.Int64
}

// Proxy is a running loopback proxy for ONE remote source.
type Proxy struct {
	opts   Options
	client *http.Client
	store  *blockStore
	ln     net.Listener
	srv    *http.Server
	prefix string // "/<token>/"
	name   string // "source<ext>"

	mu   sync.Mutex
	url  string
	size atomic.Int64 // total upstream size, -1 until the first response

	stats     counters
	closeOnce sync.Once

	// Single-upstream mode (Options.SingleUpstream): link is the one upstream
	// reader, owned by whoever holds linkSem; ctx bounds its requests to the
	// proxy's life (a client that goes away mid-block does not abort the block
	// another client may be about to reuse).
	link    *reader
	linkSem chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
}

// foreground counts in-flight Foreground requests across EVERY proxy in the
// process. Process-wide on purpose: all sessions share one uplink, and when the
// player re-creates a session the old one's bulk extractor otherwise keeps
// saturating the link while the new session starts (seen in the field: the new
// session's seek index took 10.8 s instead of 0.8 s).
var foreground atomic.Int32

// Start listens on 127.0.0.1 and serves opts.URL through the cache.
func Start(opts Options) (*Proxy, error) {
	if opts.URL == "" || opts.Dir == "" {
		return nil, errors.New("srcproxy: URL and Dir are required")
	}
	applyDefaults(&opts)
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return nil, err
	}
	p := &Proxy{opts: opts, client: opts.Client, url: opts.URL, prefix: "/" + hex.EncodeToString(token) + "/", name: "source" + sourceExt(opts.URL)}
	p.size.Store(-1)
	store, err := newBlockStore(filepath.Join(opts.Dir, "source-cache.bin"), int(opts.CacheBytes/blockSize), p.pinned)
	if err != nil {
		return nil, err
	}
	p.store = store
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		_ = store.close()
		return nil, err
	}
	p.ln = ln
	p.ctx, p.cancel = context.WithCancel(context.Background()) //nolint:gosec // G118: released by Close (p.cancel).
	if opts.SingleUpstream {
		p.link = &reader{p: p, ctx: p.ctx, stall: opts.StallTimeout}
		p.linkSem = make(chan struct{}, 1)
	}
	p.srv = &http.Server{Handler: http.HandlerFunc(p.handle), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

func applyDefaults(o *Options) {
	if o.CacheBytes <= 0 {
		o.CacheBytes = defaultCacheBytes
	}
	if o.PinHead <= 0 {
		o.PinHead = defaultPinHead
	}
	if o.PinTail <= 0 {
		o.PinTail = defaultPinTail
	}
	if o.StallTimeout <= 0 {
		o.StallTimeout = defaultLinkStall
	}
	// Pinned blocks are never evicted: keep LRU room beyond them.
	if floor := o.PinHead + o.PinTail + 32*blockSize; o.CacheBytes < floor {
		o.CacheBytes = floor
	}
	if o.Client == nil {
		tr, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			o.Client = &http.Client{}
			return
		}
		tr = tr.Clone()
		tr.ResponseHeaderTimeout = 30 * time.Second
		o.Client = &http.Client{Transport: tr} // no overall Timeout: bodies are long streams
	}
}

// sourceExt keeps the upstream extension so demuxer probing sees the same hint.
func sourceExt(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	ext := strings.ToLower(path.Ext(u.Path))
	if len(ext) > 6 || strings.ContainsAny(ext, "/\\?%") {
		return ""
	}
	return ext
}

// ForegroundURL is for latency-critical readers (segment generation, indexing).
func (p *Proxy) ForegroundURL() string { return p.base() + "fg/" + p.name }

// BackgroundURL is for bulk readers that must yield to foreground traffic.
func (p *Proxy) BackgroundURL() string { return p.base() + "bg/" + p.name }

func (p *Proxy) base() string { return "http://" + p.ln.Addr().String() + p.prefix }

// Stats returns a traffic snapshot.
func (p *Proxy) Stats() Stats {
	return Stats{
		UpstreamRequests: p.stats.upstreamRequests.Load(),
		UpstreamBytes:    p.stats.upstreamBytes.Load(),
		ServedBytes:      p.stats.servedBytes.Load(),
		CacheHitBytes:    p.stats.cacheHitBytes.Load(),
	}
}

// Close stops the listener, aborts in-flight requests and removes the cache file.
func (p *Proxy) Close() error {
	var err error
	p.closeOnce.Do(func() {
		_ = p.srv.Close()
		p.cancel()
		if p.link != nil {
			// The cancel aborts a block in flight; taking the link then closes
			// the idle upstream response, so the source connection is gone when
			// Close returns (the next reader of the account may open right after).
			p.linkSem <- struct{}{}
			p.link.close()
			<-p.linkSem
		}
		// A fully read response parks its keep-alive connection in the pool.
		p.client.CloseIdleConnections()
		err = p.store.close()
	})
	return err
}

// withLink runs fn holding the single upstream link, or returns ctx's error if
// the caller gives up waiting for it first. fn runs under the link watchdog
// (reader.guarded), so a provider that stops sending cannot keep the link —
// and every reader queued behind it — past the stall limit.
func (p *Proxy) withLink(ctx context.Context, fn func(*reader) error) error {
	select {
	case p.linkSem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-p.linkSem }()
	if err := p.ctx.Err(); err != nil {
		return err
	}
	return p.link.guarded(ctx, func() error { return fn(p.link) })
}

// fetchBlock reads block idx from upstream: through the request's own reader,
// or — single-upstream — through the shared link.
func (p *Proxy) fetchBlock(rd *reader, idx int64, buf []byte) (int, error) {
	if p.link == nil {
		return rd.fetch(idx, buf)
	}
	var n int
	err := p.withLink(rd.ctx, func(link *reader) error {
		// The link holder we queued behind may have just stored this very
		// block (a retried segment, a seek-back): serve it instead of
		// reopening the provider for bytes we already have.
		if hn, hit := p.store.get(idx, buf); hit {
			n = hn
			return nil
		}
		var ferr error
		n, ferr = link.fetch(idx, buf)
		return ferr
	})
	return n, err
}

// learnSize opens upstream at off just to learn the total size (the first
// response carries it); single-upstream it uses the shared link, which then
// sits positioned at off for the block read that follows.
func (p *Proxy) learnSize(rd *reader, off int64) error {
	if p.link == nil {
		return rd.seek(off)
	}
	return p.withLink(rd.ctx, func(link *reader) error { return link.seek(off) })
}

func (p *Proxy) liveURL() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.url
}

func (p *Proxy) pinned(idx int64) bool {
	start := idx * blockSize
	if start < p.opts.PinHead {
		return true
	}
	size := p.size.Load()
	return size > 0 && start+blockSize > size-p.opts.PinTail
}

// blockLen is the byte length of block idx (the last block is short).
func (p *Proxy) blockLen(idx int64) int {
	rest := p.size.Load() - idx*blockSize
	if rest > blockSize {
		return blockSize
	}
	if rest < 0 {
		return 0
	}
	return int(rest)
}

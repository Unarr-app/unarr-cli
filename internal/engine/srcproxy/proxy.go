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
		err = p.store.close()
	})
	return err
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

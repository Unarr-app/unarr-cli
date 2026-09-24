package srcproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

// errUpstream is returned for any unusable upstream answer (no range support,
// mismatching Content-Range, changed size). Never carries the signed URL.
var errUpstream = errors.New("srcproxy: upstream unusable")

// fetchAttempts bounds reopen-and-resume tries for one block. Debrid CDNs drop
// long-lived connections routinely; one silent resume keeps a segment alive
// instead of failing its whole ffmpeg run.
const fetchAttempts = 3

// errStalled ends a single-upstream link operation the watchdog gave up on, so
// the link is released for the next reader instead of wedging behind it.
var errStalled = errors.New("srcproxy: upstream stalled")

// reader is ONE client request's view of the upstream: a lazily opened,
// sequential ranged GET that is reused while the client keeps reading forward
// and reopened on a gap or a transport error.
//
// The single-upstream link (stall > 0) additionally runs every operation under
// a no-progress watchdog (see guarded): its reads are bound to the proxy's
// life, not to the requesting client's, so without one a block the provider
// never delivers would hold the link — and every other reader — until Close.
type reader struct {
	p     *Proxy
	ctx   context.Context
	body  io.ReadCloser
	off   int64         // upstream offset of the next byte body yields
	stall time.Duration // link only: no-progress limit per operation

	mu         sync.Mutex
	cancelBody context.CancelFunc // aborts the open request / body read
	gen        uint64             // current guarded operation
	busy       bool               // a guarded operation is running
	aborted    bool               // the watchdog fired for the current operation
	watch      *time.Timer
	limit      time.Duration // current no-progress limit (shrinks once the requester left)
}

func (rd *reader) close() {
	if rd.body != nil {
		_ = rd.body.Close()
		rd.body = nil
	}
	rd.mu.Lock()
	if rd.cancelBody != nil {
		rd.cancelBody()
		rd.cancelBody = nil
	}
	rd.mu.Unlock()
}

// fetch reads block idx from upstream into buf and returns its length.
func (rd *reader) fetch(idx int64, buf []byte) (int, error) {
	var err error
	for attempt := 0; attempt < fetchAttempts; attempt++ {
		if cerr := rd.ctx.Err(); cerr != nil {
			return 0, cerr
		}
		if rd.isAborted() {
			return 0, errStalled
		}
		if err = rd.seek(idx * blockSize); err != nil {
			continue
		}
		want := rd.p.blockLen(idx)
		if want <= 0 {
			return 0, io.EOF
		}
		if err = rd.readFull(buf[:want]); err == nil {
			rd.off += int64(want)
			rd.p.stats.upstreamBytes.Add(int64(want))
			return want, nil
		}
		rd.close()
	}
	if rd.isAborted() {
		return 0, errStalled
	}
	return 0, err
}

// readFull fills buf from the body, feeding the watchdog on every byte that
// arrives (a slow but moving source is never cut).
func (rd *reader) readFull(buf []byte) error {
	if rd.stall <= 0 {
		_, err := io.ReadFull(rd.body, buf)
		return err
	}
	for got := 0; got < len(buf); {
		n, err := rd.body.Read(buf[got:])
		got += n
		if n > 0 {
			rd.kick()
		}
		if got == len(buf) {
			return nil
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return io.ErrUnexpectedEOF
			}
			return err
		}
	}
	return nil
}

func (rd *reader) seek(off int64) error {
	if rd.body != nil && rd.off == off {
		return nil
	}
	rd.close()
	// The body lives under its own cancel, so the watchdog can abort a hung
	// open or read without cancelling the reader's context.
	bctx, cancel := context.WithCancel(rd.ctx)
	rd.mu.Lock()
	if rd.aborted {
		rd.mu.Unlock()
		cancel()
		return errStalled
	}
	rd.cancelBody = cancel
	rd.mu.Unlock()
	body, err := rd.p.open(bctx, off)
	if err != nil {
		rd.close()
		return err
	}
	rd.body, rd.off = body, off
	return nil
}

// guarded runs one link operation under the watchdog: it is aborted (the body
// cancelled, errStalled returned) when no byte arrives for rd.stall, or for
// departedStall once the requesting client (trigger) has gone away. A block
// that keeps flowing after its requester left still completes, so the next
// reader continuing from there reuses the open response.
func (rd *reader) guarded(trigger context.Context, fn func() error) error {
	rd.mu.Lock()
	rd.gen++
	g := rd.gen
	rd.busy, rd.aborted, rd.limit = true, false, rd.stall
	rd.watch = time.AfterFunc(rd.limit, func() { rd.interrupt(g) })
	rd.mu.Unlock()
	stop := context.AfterFunc(trigger, func() { rd.shorten(g) })

	err := fn()

	stop()
	rd.mu.Lock()
	rd.busy = false
	rd.watch.Stop()
	rd.watch = nil
	rd.aborted = false
	rd.mu.Unlock()
	return err
}

// kick restarts the no-progress countdown of the running operation.
func (rd *reader) kick() {
	rd.mu.Lock()
	if rd.watch != nil {
		rd.watch.Reset(rd.limit)
	}
	rd.mu.Unlock()
}

// shorten: the requester left — give the operation departedStall more of
// silence before it is abandoned.
func (rd *reader) shorten(g uint64) {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	if !rd.busy || rd.gen != g || rd.limit <= departedStall {
		return
	}
	rd.limit = departedStall
	rd.watch.Reset(departedStall)
}

// interrupt aborts operation g if it is still the one running (a timer that
// fires late, after the operation ended, is a no-op).
func (rd *reader) interrupt(g uint64) {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	if !rd.busy || rd.gen != g {
		return
	}
	rd.aborted = true
	if rd.cancelBody != nil {
		rd.cancelBody()
	}
}

func (rd *reader) isAborted() bool {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	return rd.aborted
}

// open issues `Range: bytes=off-` and validates the answer. An expired signed
// link (401/403/404/410) is re-resolved once through Options.Refresh.
func (p *Proxy) open(ctx context.Context, off int64) (io.ReadCloser, error) {
	resp, err := p.get(ctx, off)
	if err != nil {
		return nil, err
	}
	if expiredStatus(resp.StatusCode) && p.refresh(ctx) {
		_ = resp.Body.Close()
		if resp, err = p.get(ctx, off); err != nil {
			return nil, err
		}
	}
	if err := p.accept(resp, off); err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	p.stats.upstreamRequests.Add(1)
	return resp.Body, nil
}

func (p *Proxy) get(ctx context.Context, off int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.liveURL(), nil)
	if err != nil {
		return nil, errUpstream
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", off))
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", userAgent)
	var conn net.Conn
	if p.link != nil {
		req = req.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) { conn = info.Conn },
		}))
	}
	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errUpstream // url.Error would embed the signed link
	}
	if conn != nil {
		resp.Body = &connBody{ReadCloser: resp.Body, conn: conn}
	}
	return resp, nil
}

// connBody closes the response's own connection, synchronously, when the body
// is closed. Single-upstream only: abandoning a body mid-read otherwise leaves
// the socket to the transport's read loop, which closes it on another
// goroutine — so the next request could dial while the provider still sees
// the previous connection open (a one-connection account counts both; seen on
// macOS CI).
type connBody struct {
	io.ReadCloser
	conn net.Conn
}

func (b *connBody) Close() error {
	err := b.ReadCloser.Close()
	_ = b.conn.Close()
	return err
}

// accept checks the response really starts at off and pins the total size.
func (p *Proxy) accept(resp *http.Response, off int64) error {
	if resp.Header.Get("Content-Encoding") != "" {
		return errUpstream
	}
	var size int64
	switch resp.StatusCode {
	case http.StatusPartialContent:
		var first, last int64
		if _, err := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &first, &last, &size); err != nil || first != off {
			return errUpstream
		}
	case http.StatusOK:
		if off != 0 || resp.ContentLength <= 0 {
			return errUpstream
		}
		size = resp.ContentLength
	default:
		return fmt.Errorf("%w: status %d", errUpstream, resp.StatusCode)
	}
	if size <= off || !p.size.CompareAndSwap(-1, size) && p.size.Load() != size {
		return errUpstream
	}
	return nil
}

func expiredStatus(code int) bool {
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone:
		return true
	}
	return false
}

// refresh re-resolves the signed link. Reports whether a NEW url is in place.
func (p *Proxy) refresh(ctx context.Context) bool {
	if p.opts.Refresh == nil {
		return false
	}
	rctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	fresh, err := p.opts.Refresh(rctx)
	if err != nil || fresh == "" {
		return false
	}
	p.mu.Lock()
	p.url = fresh
	p.mu.Unlock()
	return true
}

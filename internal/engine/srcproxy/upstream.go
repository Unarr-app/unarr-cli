package srcproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// errUpstream is returned for any unusable upstream answer (no range support,
// mismatching Content-Range, changed size). Never carries the signed URL.
var errUpstream = errors.New("srcproxy: upstream unusable")

// fetchAttempts bounds reopen-and-resume tries for one block. Debrid CDNs drop
// long-lived connections routinely; one silent resume keeps a segment alive
// instead of failing its whole ffmpeg run.
const fetchAttempts = 3

// reader is ONE client request's view of the upstream: a lazily opened,
// sequential ranged GET that is reused while the client keeps reading forward
// and reopened on a gap or a transport error.
type reader struct {
	p    *Proxy
	ctx  context.Context
	body io.ReadCloser
	off  int64 // upstream offset of the next byte body yields
}

func (rd *reader) close() {
	if rd.body != nil {
		_ = rd.body.Close()
		rd.body = nil
	}
}

// fetch reads block idx from upstream into buf and returns its length.
func (rd *reader) fetch(idx int64, buf []byte) (int, error) {
	var err error
	for attempt := 0; attempt < fetchAttempts; attempt++ {
		if cerr := rd.ctx.Err(); cerr != nil {
			return 0, cerr
		}
		if err = rd.seek(idx * blockSize); err != nil {
			continue
		}
		want := rd.p.blockLen(idx)
		if want <= 0 {
			return 0, io.EOF
		}
		if _, err = io.ReadFull(rd.body, buf[:want]); err == nil {
			rd.off += int64(want)
			rd.p.stats.upstreamBytes.Add(int64(want))
			return want, nil
		}
		rd.close()
	}
	return 0, err
}

func (rd *reader) seek(off int64) error {
	if rd.body != nil && rd.off == off {
		return nil
	}
	rd.close()
	body, err := rd.p.open(rd.ctx, off)
	if err != nil {
		return err
	}
	rd.body, rd.off = body, off
	return nil
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
	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errUpstream // url.Error would embed the signed link
	}
	return resp, nil
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

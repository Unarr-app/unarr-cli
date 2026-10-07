package remotefs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HTTPClient uses the standard dialer/proxy/HTTP2 defaults with a pool large
// enough for concurrent playback. HTTPReader bounds body inactivity separately.
func HTTPClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 64
	t.MaxIdleConnsPerHost = 16
	t.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{Transport: t}
}

// Link coalesces resolution and renewal for every handle of the same file.
type Link struct {
	mu        sync.Mutex
	url       string
	flight    chan struct{}
	failedAt  time.Time
	renewedAt time.Time
	err       error
	Resolve   func(context.Context) (string, error)
}

func (l *Link) get(ctx context.Context, rejected string) (string, error) {
	for {
		l.mu.Lock()
		if l.url != "" && (l.url != rejected || time.Since(l.renewedAt) < 2*time.Second) {
			u := l.url
			l.mu.Unlock()
			return u, nil
		}
		if l.err != nil && time.Since(l.failedAt) < 2*time.Second {
			err := l.err
			l.mu.Unlock()
			return "", err
		}
		if done := l.flight; done != nil {
			l.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		l.flight = make(chan struct{})
		l.mu.Unlock()
		return l.resolve(ctx, rejected != "")
	}
}

func (l *Link) resolve(ctx context.Context, renewal bool) (string, error) {
	var u string
	err := errors.New("no remote URL resolver")
	if l.Resolve != nil {
		u, err = l.Resolve(ctx)
	}
	if err == nil {
		err = validateHTTPURL(u)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err == nil {
		l.url = u
		if renewal {
			l.renewedAt = time.Now()
		}
	}
	l.err = err
	if err != nil {
		l.failedAt = time.Now()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		l.err = nil
	}
	close(l.flight)
	l.flight = nil
	return u, err
}

func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("provider returned an invalid delivery URL")
	}
	return nil
}

type rangeKey struct{}

func withRange(ctx context.Context, r string) context.Context {
	return context.WithValue(ctx, rangeKey{}, r)
}

// rangeEnd bounds the upstream body to the HTTP client's requested interval.
// This allows completed small ranges to reuse their TCP connection.
func rangeEnd(ctx context.Context, start, size int64) int64 {
	end := size - 1
	h, _ := ctx.Value(rangeKey{}).(string)
	if !strings.HasPrefix(h, "bytes=") || strings.Contains(h, ",") {
		return end
	}
	p := strings.SplitN(strings.TrimPrefix(h, "bytes="), "-", 2)
	if len(p) != 2 {
		return end
	}
	if p[0] == "" {
		return end
	}
	a, e1 := strconv.ParseInt(strings.TrimSpace(p[0]), 10, 64)
	b, e2 := strconv.ParseInt(strings.TrimSpace(p[1]), 10, 64)
	if e1 == nil && e2 == nil && a <= start && b >= start && b < end {
		return b
	}
	return end
}

// HTTPReader streams sequentially from a single ranged response, reopens only
// on seek/EOF, and validates Content-Range before exposing any bytes.
type HTTPReader struct {
	ctx                        context.Context
	client                     *http.Client
	link                       *Link
	size, pos, bodyAt, bodyEnd int64
	body                       io.ReadCloser
	closed                     bool
	bodyIdleTimeout            time.Duration
}

func NewHTTPReader(ctx context.Context, client *http.Client, link *Link, size int64) *HTTPReader {
	return &HTTPReader{ctx: ctx, client: client, link: link, size: size, bodyIdleTimeout: mediaIdleTimeout}
}
func (r *HTTPReader) Close() error {
	r.closed = true
	return r.dropBody()
}
func (r *HTTPReader) dropBody() error {
	if r.body == nil {
		return nil
	}
	err := r.body.Close()
	r.body = nil
	return err
}
func (r *HTTPReader) Seek(off int64, whence int) (int64, error) {
	if r.closed {
		return 0, errors.New("remote reader closed")
	}
	p, err := seekPosition(r.pos, r.size, off, whence)
	if err != nil {
		return 0, err
	}
	if p != r.pos {
		_ = r.dropBody()
	}
	r.pos = p
	return p, nil
}
func (r *HTTPReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, errors.New("remote reader closed")
	}
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.pos >= r.size {
		return 0, io.EOF
	}
	if r.body == nil {
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	if remaining := r.bodyEnd - r.pos + 1; int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := r.body.Read(p)
	r.pos += int64(n)
	if r.pos > r.bodyEnd {
		_ = r.dropBody()
		if err == io.EOF && r.pos < r.size {
			err = nil
		}
	} else if err == io.EOF {
		_ = r.dropBody()
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func (r *HTTPReader) open() error {
	u, err := r.link.get(r.ctx, "")
	if err != nil {
		return err
	}
	end := rangeEnd(r.ctx, r.pos, r.size)
	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel := context.WithCancelCause(r.ctx)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			cancel(context.Canceled)
			return errors.New("invalid remote request")
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", r.pos, end))
		req.Header.Set("Accept-Encoding", "identity")
		resp, err := r.client.Do(req)
		if err != nil {
			cancel(context.Canceled)
			return remoteRequestError(r.ctx)
		}
		if expiredStatus(resp.StatusCode) && attempt == 0 {
			cancel(context.Canceled)
			_ = resp.Body.Close()
			u, err = r.link.get(r.ctx, u)
			if err != nil {
				return err
			}
			continue
		}
		if err = checkRange(resp, r.pos, end, r.size); err != nil {
			cancel(context.Canceled)
			_ = resp.Body.Close()
			return err
		}
		r.body = &idleBody{ReadCloser: resp.Body, ctx: ctx, cancel: cancel, timeout: r.bodyIdleTimeout}
		r.bodyAt, r.bodyEnd = r.pos, end
		return nil
	}
	return errors.New("remote link renewal failed")
}

func remoteRequestError(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("remote HTTP request failed")
}

func expiredStatus(s int) bool { return s == 401 || s == 403 || s == 404 || s == 410 }
func checkRange(resp *http.Response, start, end, size int64) error {
	if resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity" {
		return errors.New("remote response has unexpected encoding")
	}
	if resp.StatusCode == http.StatusOK && start == 0 && end == size-1 && resp.ContentLength == size {
		return nil
	}
	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("remote HTTP status %d: range unavailable", resp.StatusCode)
	}
	want := fmt.Sprintf("bytes %d-%d/%d", start, end, size)
	if resp.Header.Get("Content-Range") != want {
		return errors.New("remote Content-Range differs from requested file bytes")
	}
	if resp.ContentLength >= 0 && resp.ContentLength != end-start+1 {
		return errors.New("remote Content-Length mismatch")
	}
	return nil
}

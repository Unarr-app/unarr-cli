// Package engine — stream_source_debrid.go implements a FileProvider that
// serves a /stream session straight from a debrid HTTPS direct URL (hueco #2 /
// 2a). No local file is involved: the browser's Range requests are translated
// into ranged GETs against the debrid link, so a cache-confirmed torrent plays
// instantly without ever hitting the swarm or touching disk.
//
// The web resolves the DirectURL server-side (resolveDebridDirectUrl) and only
// sends it when the hash is debrid-cached and the container is browser-native
// (mp4/m4v), so this provider stays a pure pass-through — same role as
// diskFileProvider/torrentFileProvider, just backed by HTTP Range instead of a
// file handle. http.ServeContent drives it exactly like a local file: it Seeks
// to discover size + the range start (no network), then Reads (lazy GET).
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"strings"
	"time"
)

// debridHTTPClient is used for ranged debrid reads. Separate from the download
// httpClient so a slow streaming read can't starve a concurrent download's
// header-timeout budget, and vice versa. No overall timeout: a paused player
// can legitimately hold a body open for minutes; ResponseHeaderTimeout bounds
// the part that actually matters (a hung server before first byte).
var debridHTTPClient = &http.Client{
	Transport: &http.Transport{
		ResponseHeaderTimeout: 30 * time.Second,
		// debrid CDNs are remote; a generous idle-conn pool avoids a fresh TLS
		// handshake on every seek-driven reopen.
		MaxIdleConns:        4,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
	},
}

// NewDebridFileProvider builds a FileProvider backed by a debrid HTTPS URL.
// It performs a single HEAD up front to learn the exact file size (the torrent
// size the web knows can differ from the resolved file's size). If the HEAD
// fails or omits Content-Length, fallbackSize (from the StreamSession) is used.
// Returns an error only when neither a HEAD size nor a fallback is available —
// http.ServeContent needs a real size to range-serve, and serving size 0 would
// hand the browser an empty file.
func NewDebridFileProvider(ctx context.Context, directURL, fileName string, fallbackSize int64) (FileProvider, error) {
	if directURL == "" {
		return nil, errors.New("debrid provider: empty direct URL")
	}
	size := fallbackSize
	if headSize, ok := debridHeadSize(ctx, directURL); ok {
		size = headSize
	}
	if size <= 0 {
		return nil, fmt.Errorf("debrid provider: unknown file size (HEAD gave nothing, no fallback)")
	}
	// The name drives the served Content-Type (mimeTypeFromExt on FileName).
	// The web may pass a torrent title with no extension (its file-name
	// fallback), which would yield application/octet-stream and break <video>
	// on strict clients (Safari). The debrid URL reliably ends in the real
	// file name *with* its extension, so derive from it whenever the passed
	// name lacks one.
	name := fileName
	if name == "" || path.Ext(name) == "" {
		name = debridNameFromURL(directURL)
	}
	return &debridFileProvider{
		url:  directURL,
		name: name,
		size: size,
	}, nil
}

// debridFileProvider serves a file from a debrid HTTPS URL via ranged GETs.
type debridFileProvider struct {
	url  string
	name string
	size int64
}

func (p *debridFileProvider) NewFileReader(ctx context.Context) io.ReadSeekCloser {
	return &debridRangeReader{
		ctx:  ctx,
		url:  p.url,
		size: p.size,
	}
}

func (p *debridFileProvider) FileName() string { return p.name }
func (p *debridFileProvider) FileSize() int64  { return p.size }

// debridRangeReader is an io.ReadSeekCloser over an HTTP resource that supports
// Range. Seek is network-free: it only moves the logical position. Read opens
// (or reuses) a GET starting at the current position and streams the body; a
// Seek that moves away from the open body's cursor forces a reopen on the next
// Read. This matches how http.ServeContent works — Seek(0, SeekEnd) for size,
// Seek to the range start, then sequential Reads — so seeks the user makes in
// the player become a single reopened GET, never a full re-download.
type debridRangeReader struct {
	ctx  context.Context
	url  string
	size int64

	pos    int64         // logical position (moved by Seek, advanced by Read)
	body   io.ReadCloser // current open response body, or nil
	bodyAt int64         // position the open body's next byte maps to
}

func (r *debridRangeReader) Read(p []byte) (int, error) {
	if r.size > 0 && r.pos >= r.size {
		return 0, io.EOF
	}
	// (Re)open when no body is held or a Seek moved us off the open body.
	if r.body == nil || r.pos != r.bodyAt {
		if err := r.reopen(); err != nil {
			return 0, err
		}
	}
	n, err := r.body.Read(p)
	r.pos += int64(n)
	r.bodyAt = r.pos
	if err == io.EOF {
		// Body drained. Drop it so the next Read reopens (covers a server that
		// closed the connection before the logical EOF). Surface EOF to the
		// caller only when we've actually reached end-of-file; otherwise hand
		// back the bytes read with no error and let the caller Read again.
		_ = r.body.Close()
		r.body = nil
		if r.size > 0 && r.pos < r.size {
			return n, nil
		}
	}
	return n, err
}

func (r *debridRangeReader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.pos + offset
	case io.SeekEnd:
		abs = r.size + offset
	default:
		return 0, fmt.Errorf("debrid reader: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, errors.New("debrid reader: negative position")
	}
	r.pos = abs
	return abs, nil
}

func (r *debridRangeReader) Close() error {
	if r.body != nil {
		err := r.body.Close()
		r.body = nil
		return err
	}
	return nil
}

// reopen issues a fresh ranged GET from the current logical position. Closes
// any previously held body first.
func (r *debridRangeReader) reopen() error {
	if r.body != nil {
		_ = r.body.Close()
		r.body = nil
	}
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return fmt.Errorf("debrid reader: build request: %w", err)
	}
	// Always send a Range so a seek to 0 still gets a 206 (and so partial
	// reopens after a mid-file seek work). An open-ended range runs to EOF.
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", r.pos))
	resp, err := debridHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("debrid reader: GET: %w", err)
	}
	switch resp.StatusCode {
	case http.StatusPartialContent:
		// Expected.
	case http.StatusOK:
		// Server ignored Range and is sending the whole file from 0. Only valid
		// when we asked from 0; otherwise the bytes wouldn't line up with pos.
		if r.pos != 0 {
			resp.Body.Close()
			return fmt.Errorf("debrid reader: server ignored Range at offset %d (got 200)", r.pos)
		}
	case http.StatusRequestedRangeNotSatisfiable:
		resp.Body.Close()
		return io.EOF // seeked past end — treat as EOF, not a hard error
	default:
		resp.Body.Close()
		return fmt.Errorf("debrid reader: unexpected status %d %s", resp.StatusCode, resp.Status)
	}
	r.body = resp.Body
	r.bodyAt = r.pos
	return nil
}

// debridHeadSize issues a HEAD and returns the Content-Length when present.
// Best-effort: any failure returns (0, false) so the caller falls back to the
// size the web reported. A short timeout keeps a slow/HEAD-hostile CDN from
// stalling session setup — the fallback size is good enough to start.
func debridHeadSize(ctx context.Context, url string) (int64, bool) {
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(hctx, http.MethodHead, url, nil)
	if err != nil {
		return 0, false
	}
	resp, err := debridHTTPClient.Do(req)
	if err != nil {
		log.Printf("[stream] debrid HEAD failed (using fallback size): %v", err)
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength <= 0 {
		return 0, false
	}
	return resp.ContentLength, true
}

// debridNameFromURL extracts a filename from a URL path as a last resort when
// the server didn't send one. Strips query/fragment via path.Base on the path.
func debridNameFromURL(rawURL string) string {
	u := rawURL
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	base := path.Base(u)
	if base == "" || base == "." || base == "/" {
		return "video.mp4"
	}
	return base
}

// Package engine — stream_usenet_failure.go turns a Usenet read failure into an
// HTTP outcome a consumer can act on, instead of a 206 whose body silently stops.
//
// http.ServeContent commits the status and headers before its first Read, so a
// Read error can only ever truncate the body. Two cases are therefore split:
//
//   - The FIRST byte of the requested range cannot be produced (its article is
//     dead or stalled, or the fetch fails outright): checked BEFORE ServeContent
//     by reading that byte, and answered with an error status and no body.
//   - A failure after bytes were written: the response is ABORTED
//     (http.ErrAbortHandler) and logged once, so the client sees a broken
//     connection — never a short body that ends as if it were complete.
package engine

import (
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/usenet/stream"
)

// usenetRangeStart returns the offset of the first byte a GET for size bytes
// will serve: 0 without a Range header, else the first range spec's start
// (suffix ranges resolved against size). ok is false for a header ServeContent
// will reject or ignore anyway — the check is skipped and ServeContent answers.
func usenetRangeStart(header string, size int64) (int64, bool) {
	if header == "" {
		return 0, true
	}
	spec, ok := strings.CutPrefix(header, "bytes=")
	if !ok {
		return 0, false
	}
	first, _, _ := strings.Cut(spec, ",")
	startStr, endStr, ok := strings.Cut(strings.TrimSpace(first), "-")
	if !ok {
		return 0, false
	}
	if startStr == "" { // suffix range: the last N bytes
		n, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil || n <= 0 {
			return 0, false
		}
		return max(size-n, 0), true
	}
	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil || start < 0 {
		return 0, false
	}
	return start, true
}

// checkUsenetRangeStart reads the first byte the request will be served, before
// any header is written. The article it fetches lands in the source's shared
// cache, so ServeContent's own read of it costs nothing more. HEAD requests and
// unsatisfiable or unparsable ranges are left to ServeContent.
func checkUsenetRangeStart(r *http.Request, rd io.ReadSeeker, size int64) error {
	if r.Method == http.MethodHead {
		return nil
	}
	start, ok := usenetRangeStart(r.Header.Get("Range"), size)
	if !ok || start >= size {
		return nil
	}
	if _, err := rd.Seek(start, io.SeekStart); err != nil {
		return err
	}
	var b [1]byte
	if _, err := rd.Read(b[:]); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// failUsenetRequest answers a request whose first byte cannot be produced.
//
// Status choice:
//   - 502 Bad Gateway for a dead article (430/423) or any other upstream fetch
//     failure: we are a gateway to NNTP and the upstream gave a definitive failure.
//     Not 503: that promises a retry would help, and for a missing article it will
//     not. Not 404/416: the resource and the range are both valid.
//   - 504 Gateway Timeout for a stalled article: the upstream did not answer in
//     time. Retry-After is the memo's TTL, after which it will be asked again.
//
// ffmpeg treats a 5xx on its input as fatal (it only reconnects on transport
// errors unless -reconnect_on_http_error is set), so the HLS job fails at once
// instead of reconnect-looping into the same hole. A client that has already
// gone away gets nothing.
func failUsenetRequest(w http.ResponseWriter, r *http.Request, id string, err error) {
	if r.Context().Err() != nil {
		return
	}
	status := http.StatusBadGateway
	var u *stream.ArticleUnavailableError
	if errors.As(err, &u) {
		w.Header().Set("X-Unarr-Missing-Article", u.MessageID)
		if u.Stalled {
			status = http.StatusGatewayTimeout
			w.Header().Set("Retry-After", strconv.Itoa(int(stream.StalledArticleTTL.Seconds())))
		}
	}
	log.Printf("[usenet-stream] /usenet/%s %s: first byte of range unavailable, answering %d: %v",
		id, r.Header.Get("Range"), status, err)
	http.Error(w, http.StatusText(status), status)
}

// usenetReadRecorder passes reads through, remembering the first real read
// failure and the offset it happened at.
type usenetReadRecorder struct {
	io.ReadSeeker
	pos    int64
	err    error
	errPos int64
}

func (rec *usenetReadRecorder) Seek(offset int64, whence int) (int64, error) {
	pos, err := rec.ReadSeeker.Seek(offset, whence)
	if err == nil {
		rec.pos = pos
	}
	return pos, err
}

func (rec *usenetReadRecorder) Read(p []byte) (int, error) {
	n, err := rec.ReadSeeker.Read(p)
	rec.pos += int64(n)
	if err != nil && !errors.Is(err, io.EOF) && rec.err == nil {
		rec.err, rec.errPos = err, rec.pos
	}
	return n, err
}

// abortOnUsenetReadFailure aborts a response whose body failed part-way. By then
// ServeContent has written the status and part of the body, and returning
// normally would end the response with fewer bytes than its Content-Length. The
// abort makes the outcome deterministic — the connection is torn down, which
// ffmpeg's -reconnect re-requests from the last byte it got (and then hits the
// clean pre-body failure above) — and it is logged exactly once. A client that
// disconnected itself is not a failure.
func abortOnUsenetReadFailure(r *http.Request, id string, rec *usenetReadRecorder) {
	if rec.err == nil || r.Context().Err() != nil {
		return
	}
	log.Printf("[usenet-stream] aborting /usenet/%s response at offset %d (Range %q): %v",
		id, rec.errPos, r.Header.Get("Range"), rec.err)
	panic(http.ErrAbortHandler)
}

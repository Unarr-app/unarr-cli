package remotefs

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

// Bound inactivity, not total movie duration. Upstream time is counted only
// while reading; slow consumers are bounded separately at the DAV socket write.
const mediaIdleTimeout = 30 * time.Second

var errBodyIdle = errors.New("remote HTTP body made no progress")

type idleBody struct {
	io.ReadCloser
	ctx     context.Context
	cancel  context.CancelCauseFunc
	timeout time.Duration
}

func (b *idleBody) Read(p []byte) (int, error) {
	done := make(chan struct{})
	timer := time.AfterFunc(b.timeout, func() { b.cancel(errBodyIdle); close(done) })
	n, err := b.ReadCloser.Read(p)
	if !timer.Stop() {
		<-done
	}
	if cause := context.Cause(b.ctx); cause != nil {
		return n, cause
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.cancel(context.Canceled)
	return b.ReadCloser.Close()
}

type progressWriter struct {
	http.ResponseWriter
	timeout time.Duration
}

func (w progressWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w progressWriter) Write(p []byte) (int, error) {
	c := http.NewResponseController(w.ResponseWriter)
	_ = c.SetWriteDeadline(time.Now().Add(w.timeout))
	defer c.SetWriteDeadline(time.Time{})
	return w.ResponseWriter.Write(p)
}

func (w progressWriter) Flush() {
	c := http.NewResponseController(w.ResponseWriter)
	_ = c.SetWriteDeadline(time.Now().Add(w.timeout))
	defer c.SetWriteDeadline(time.Time{})
	_ = c.Flush()
}

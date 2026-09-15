package nntp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// IsArticleMissing reports whether err is the server's final "no such article"
// answer (430, or 423 from servers that answer a message-id with the by-number
// code). It is a property of the ARTICLE, not of the connection: asking again —
// on the same connection or a fresh one — gets the same answer.
func IsArticleMissing(err error) bool {
	var nf *ArticleNotFoundError
	return errors.As(err, &nf)
}

// StallError reports an article stall: BODY was accepted on a connection proven
// alive (freshly dialled, see Client.Body) and its status line did not arrive
// within the stall bound (WithStallTimeout).
type StallError struct {
	MessageID string
	Err       error
}

func (e *StallError) Error() string {
	return fmt.Sprintf("nntp: BODY <%s> not answered within the stall bound: %v", e.MessageID, e.Err)
}

func (e *StallError) Unwrap() error { return e.Err }

// IsStalled reports whether err is an article stall (*StallError). Connection
// level timeouts are NOT stalls: a dial timing out, a pooled connection that was
// silently dead, a body transfer timing out after "222", or the caller's context
// ending are transport failures, retryable and never a verdict on the article.
func IsStalled(err error) bool {
	var se *StallError
	return errors.As(err, &se)
}

func isNetTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

type stallTimeoutKey struct{}

// WithStallTimeout bounds how long Body waits for the server's STATUS LINE on
// requests made with the returned context. A server that stalls instead of
// answering 430 for a dead article otherwise holds the call for the full 60 s
// command deadline. The bound covers only the status line: once the server has
// said 222, the body transfer keeps the regular deadline, so a slow but live link
// is not cut off. A status-line timeout is retried once on a freshly dialled
// connection like any connection failure; only a timeout there is reported as a
// *StallError, so a stall costs at most two bounds plus a dial.
//
// The streaming path sets it; the batch downloader does not. d <= 0 leaves ctx
// unchanged.
func WithStallTimeout(ctx context.Context, d time.Duration) context.Context {
	if d <= 0 {
		return ctx
	}
	return context.WithValue(ctx, stallTimeoutKey{}, d)
}

func stallTimeoutFrom(ctx context.Context) (time.Duration, bool) {
	d, ok := ctx.Value(stallTimeoutKey{}).(time.Duration)
	return d, ok && d > 0
}

// statusDeadline is the connection deadline while waiting for the status line:
// the stall bound when one is set and earlier than the command deadline. bounded
// reports that the stall bound is the one in force, so its expiry is a stall.
func statusDeadline(ctx context.Context, command time.Time) (deadline time.Time, bounded bool) {
	if d, ok := stallTimeoutFrom(ctx); ok {
		if s := time.Now().Add(d); s.Before(command) {
			return s, true
		}
	}
	return command, false
}

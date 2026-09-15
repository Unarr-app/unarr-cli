package nntp

import "context"

// BodyProgress follows an article body while BodyInto reads it
// (WithBodyProgress), so a caller can use the front of an article before its end
// has arrived.
type BodyProgress interface {
	// Line is called after each line of the body is read, with the body so far:
	// what BodyInto would return if the article ended there, each line ending in
	// '\n'. It runs on the reading goroutine between reads from the connection, so
	// it must be quick. The bytes of lines already reported are never read or
	// written again, except to be copied whole into a larger array when the body
	// outgrows its buffer, so the callee may rewrite them in place.
	Line(body []byte)
	// Restart says the lines reported so far are void: the read failed and is
	// about to be retried on a new connection, from the start of the same buffer.
	Restart()
}

type bodyProgressKey struct{}

// WithBodyProgress reports the bodies read with the returned context to p.
func WithBodyProgress(ctx context.Context, p BodyProgress) context.Context {
	return context.WithValue(ctx, bodyProgressKey{}, p)
}

func bodyProgressFrom(ctx context.Context) BodyProgress {
	p, _ := ctx.Value(bodyProgressKey{}).(BodyProgress)
	return p
}

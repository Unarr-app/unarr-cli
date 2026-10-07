package nntp

import (
	"context"
	"net"
)

// The callback owns only this held socket. Closing interrupts reads and writes
// even if another phase changes its deadline; stop joins a running callback
// before the socket can be returned to the pool or handed to the caller.
func interruptOnCancel(ctx context.Context, raw net.Conn) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = raw.Close(); close(done) })
	return func() {
		if !stop() {
			<-done
		}
	}
}

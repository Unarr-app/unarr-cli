package remotefs

import "sync/atomic"

// Process-wide read counters for the daemon's session view. Package-level
// because the WebDAV handler is built deep inside the mount and there is at
// most one mount per daemon; two atomics are the whole cost on the read path.
var (
	activeReads atomic.Int64
	servedBytes atomic.Int64
)

// ReadStats reports the WebDAV reads in flight right now and the bytes served
// to the mount since the process started.
func ReadStats() (active, bytes int64) {
	return activeReads.Load(), servedBytes.Load()
}

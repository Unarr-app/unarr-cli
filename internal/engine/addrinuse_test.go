package engine

import (
	"errors"
	"net"
	"testing"
)

// TestIsAddrInUseRecognisesARealCollision makes the OS produce the error rather
// than constructing one, which is the only version of this test that means
// anything: the bug it guards was a check that matched the POSIX wording of
// EADDRINUSE and therefore never fired on Windows, where the identical failure
// is WSAEADDRINUSE with a different (and localised) message. A hand-built
// error would have been written in whatever spelling the check already knew.
//
// It also pins the assumption isAddrInUse depends on — that net wraps the
// syscall error with %w, so errors.Is can reach it.
func TestIsAddrInUseRecognisesARealCollision(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer held.Close()

	second, err := net.Listen("tcp", held.Addr().String())
	if err == nil {
		second.Close()
		t.Fatal("second listen on the same address succeeded; nothing collided")
	}
	if !isAddrInUse(err) {
		t.Errorf("isAddrInUse(%v) = false, want true — NewTorrentDownloader's port "+
			"walk is dead on this platform and a busy 42069 aborts the download", err)
	}
}

// TestIsAddrInUseIgnoresOtherFailures is the counterfactual: without it, an
// isAddrInUse that returned true unconditionally would pass the test above, and
// the port walk would swallow errors it cannot fix — retrying ten ports against
// a failure that has nothing to do with the port.
func TestIsAddrInUseIgnoresOtherFailures(t *testing.T) {
	for _, err := range []error{
		nil,
		errors.New("create torrent client: no such device"),
		&net.OpError{Op: "listen", Err: errors.New("permission denied")},
	} {
		if isAddrInUse(err) {
			t.Errorf("isAddrInUse(%v) = true, want false", err)
		}
	}
}

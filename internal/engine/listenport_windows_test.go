package engine

import (
	"fmt"
	"net"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

// TestNextListenPortJumpsAnExcludedRange builds the error exactly as the Windows
// runner produced it — anacrolix wrapping net's *OpError around the Winsock errno
// — and requires the walk to jump a whole reservation block, not one port.
func TestNextListenPortJumpsAnExcludedRange(t *testing.T) {
	bind := &net.OpError{Op: "listen", Net: "udp4",
		Err: os.NewSyscallError("bind", windows.WSAEACCES)}
	err := fmt.Errorf("subsequent listen: %w", bind)

	next, retry := nextListenPort(50689, err)
	if !retry || next != 50689+forbiddenPortStep {
		t.Errorf("nextListenPort(%v) = %d, %v; want %d, true", err, next, retry, 50689+forbiddenPortStep)
	}
	if isAddrInUse(err) {
		t.Error("a forbidden port read as a taken one: the walk would crawl through the reserved block")
	}
}

package engine

import (
	"errors"
	"fmt"
	"net"
	"testing"
)

// TestNextListenPortStepsPastATakenPort: a real collision walks to the neighbour,
// and anything a port change cannot fix stops the walk.
func TestNextListenPortStepsPastATakenPort(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer held.Close()
	_, collision := net.Listen("tcp", held.Addr().String())
	if collision == nil {
		t.Fatal("second listen on the same address succeeded; nothing collided")
	}

	if next, retry := nextListenPort(42069, fmt.Errorf("first listen: %w", collision)); !retry || next != 42070 {
		t.Errorf("taken port: nextListenPort = %d, %v; want 42070, true", next, retry)
	}
	if next, retry := nextListenPort(42069, errors.New("create torrent client: no such device")); retry || next != 42069 {
		t.Errorf("unrelated failure: nextListenPort = %d, %v; want 42069, false", next, retry)
	}
}

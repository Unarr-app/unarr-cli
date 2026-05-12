//go:build !windows

package agent

import (
	"os"
	"testing"
)

func TestIsProcessAliveSelf(t *testing.T) {
	if !IsProcessAlive(os.Getpid()) {
		t.Errorf("self PID should be alive")
	}
}

func TestIsProcessAliveBogus(t *testing.T) {
	// PID 0 is reserved (signal 0 to PID 0 broadcasts to the whole pgrp).
	// Pick a very high PID unlikely to exist.
	if IsProcessAlive(0x7FFFFFFE) {
		t.Errorf("very high PID should not be alive")
	}
}

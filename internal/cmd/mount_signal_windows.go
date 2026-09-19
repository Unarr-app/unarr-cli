//go:build windows

package cmd

import "os"

func mountSignals() []os.Signal { return []os.Signal{os.Interrupt} }

// Go does not support os.Interrupt for Windows child processes. WinFsp releases
// a process-owned mount when the child exits.
func interruptMountProcess(p *os.Process) error { return p.Kill() }

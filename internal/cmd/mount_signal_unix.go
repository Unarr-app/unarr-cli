//go:build !windows

package cmd

import (
	"os"
	"syscall"
)

func mountSignals() []os.Signal                 { return []os.Signal{os.Interrupt, syscall.SIGTERM} }
func interruptMountProcess(p *os.Process) error { return p.Signal(os.Interrupt) }

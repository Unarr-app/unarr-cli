//go:build !linux

package mediainfo

import "os/exec"

// These are Linux-specific optimizations / safeguards; no-ops elsewhere.
func setIdleIOPriority(_ int) {}
func setLowCPUPriority(_ int) {}
func hardenCmd(_ *exec.Cmd)   {}

// LoadAverage1 is unavailable off Linux; ok=false means callers don't gate.
func LoadAverage1() (float64, bool) { return 0, false }

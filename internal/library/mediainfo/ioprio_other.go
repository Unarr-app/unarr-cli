//go:build !linux

package mediainfo

// setIdleIOPriority is a no-op on non-Linux platforms (ioprio is Linux-specific).
func setIdleIOPriority(_ int) {}

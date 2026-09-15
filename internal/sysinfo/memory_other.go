//go:build !linux && !darwin && !windows

package sysinfo

func platformTotalMemory() (uint64, bool) { return 0, false }

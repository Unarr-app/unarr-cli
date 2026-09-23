//go:build !windows

package cmd

// launchedByTaskShim is Windows-only: there is no scheduled-task shim elsewhere.
func launchedByTaskShim() bool { return false }

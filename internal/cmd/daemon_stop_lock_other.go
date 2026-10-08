//go:build !windows

package cmd

func prepareMissingWindowsUninstallLockParent() error { return nil }

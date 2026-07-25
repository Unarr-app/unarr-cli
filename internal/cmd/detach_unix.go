//go:build !windows

package cmd

import "syscall"

// detachedSysProcAttr puts the child in a new session, so the SIGHUP the tty
// sends to its foreground process group when the terminal closes never reaches
// the daemon.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

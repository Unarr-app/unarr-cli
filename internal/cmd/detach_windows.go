//go:build windows

package cmd

import "syscall"

const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
)

// detachedSysProcAttr detaches the child from this console, so closing the
// window (or Ctrl+C in it) does not take the daemon down with it.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | detachedProcess}
}

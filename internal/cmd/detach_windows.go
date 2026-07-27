//go:build windows

package cmd

import "syscall"

const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
	createNoWindow        = 0x08000000
)

// detachedSysProcAttr detaches the child from this console, so closing the
// window (or Ctrl+C in it) does not take the daemon down with it.
//
// createNoWindow suppresses the fresh console the daemon would otherwise
// allocate: DETACHED_PROCESS only unbinds it from the PARENT console — a
// console-subsystem binary (unarr is not built -H=windowsgui) still spawns
// its own window on start. Without this flag a terminal pops up every time
// the tray (or `unarr start`) launches the agent on Windows.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNewProcessGroup | detachedProcess | createNoWindow}
}

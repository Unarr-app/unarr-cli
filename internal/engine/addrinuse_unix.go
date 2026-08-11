//go:build !windows

package engine

import "syscall"

// errAddrInUse is what a bind onto a taken port returns on every non-Windows
// platform: POSIX EADDRINUSE. See isAddrInUse for why this is not a string.
var errAddrInUse error = syscall.EADDRINUSE

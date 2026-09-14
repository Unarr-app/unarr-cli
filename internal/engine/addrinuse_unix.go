//go:build !windows

package engine

import "syscall"

// errAddrInUse is what a bind onto a taken port returns on every non-Windows
// platform: POSIX EADDRINUSE. See isAddrInUse for why this is not a string.
var errAddrInUse error = syscall.EADDRINUSE

// errPortForbidden is nil: there are no excluded port ranges outside Windows,
// and an EACCES here (a port below 1024 without the capability) is not fixed by
// walking to a neighbour of a port above it. See isPortForbidden.
var errPortForbidden error

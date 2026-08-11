package engine

import "golang.org/x/sys/windows"

// errAddrInUse is what a bind onto a taken port returns on Windows.
//
// Winsock reports WSAEADDRINUSE (10048), a DIFFERENT errno from the POSIX
// EADDRINUSE the same failure carries everywhere else — which is why
// isAddrInUse compares against a per-platform value instead of one constant.
//
// It comes from x/sys/windows, not syscall: the standard library's syscall
// package declares only a handful of WSA constants on Windows and this is not
// among them (`undefined: syscall.WSAEADDRINUSE`). x/sys is already a
// dependency, and taking the number from it beats writing 10048 here.
var errAddrInUse error = windows.WSAEADDRINUSE

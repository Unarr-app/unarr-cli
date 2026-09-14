package engine

import (
	"errors"
	"strings"
)

// isAddrInUse reports whether err is a bind onto an already-taken port.
//
// This deliberately does NOT match a message. The check here used to be
// strings.Contains(err, "address already in use"), which is the POSIX text and
// nothing else: Windows renders the very same failure as "Only one usage of
// each socket address (protocol/network address/port) is normally permitted.",
// and renders it LOCALISED, so a Spanish install says something different
// again. The consequence was not cosmetic — the port walk in
// NewTorrentDownloader was dead on Windows. A busy 42069 (another client, a
// previous unarr still shutting down, a socket in TIME_WAIT after a restart)
// aborted the download outright where linux and macOS quietly stepped to 42070.
// Measured on a CI runner: "first listen: listen tcp4 :42069: bind: Only one
// usage of each socket address...", returned as a hard error.
//
// errAddrInUse is per-platform because the errno is per-platform too: Winsock
// reports WSAEADDRINUSE, everyone else reports EADDRINUSE.
//
// The text check stays as a fallback, and only for the POSIX spelling: an
// intermediate layer that formats its cause with %v rather than %w breaks the
// error chain, and losing the port walk on linux to gain it on Windows would
// not be a fix.
func isAddrInUse(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, errAddrInUse) ||
		strings.Contains(err.Error(), "address already in use")
}

// isPortForbidden reports whether err is a bind the OS refused on POLICY, not
// because another socket holds the port.
//
// On Windows that is WSAEACCES (10013), "An attempt was made to access a socket
// in a way forbidden by its access permissions": the port sits inside an
// excluded port range that Hyper-V, WinNAT, WSL or Docker Desktop reserved
// (`netsh int ipv4 show excludedportrange protocol=udp`). anacrolix binds TCP
// and then UDP (uTP) on the SAME number, so a UDP reservation alone is enough.
// Seen on GitHub's windows runners as "subsequent listen: listen udp4 :50689:
// bind: An attempt was made to access a socket...", which the port walk used to
// treat as fatal: on a machine whose reservation covers 42069 the torrent engine
// would never start at all.
//
// errPortForbidden is nil outside Windows, where no such reservation exists, so
// this never fires there.
func isPortForbidden(err error) bool {
	return err != nil && errPortForbidden != nil && errors.Is(err, errPortForbidden)
}

// forbiddenPortStep is how far the walk jumps past a forbidden port. Windows
// reserves excluded ranges in blocks (commonly 50 or 100 ports), so stepping by
// one would spend every attempt inside the same block.
const forbiddenPortStep = 100

// nextListenPort returns the port to try after a failed client start, and
// whether a different port can fix err at all.
func nextListenPort(port int, err error) (int, bool) {
	switch {
	case isAddrInUse(err):
		return port + 1, true
	case isPortForbidden(err):
		return port + forbiddenPortStep, true
	}
	return port, false
}

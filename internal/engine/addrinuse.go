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

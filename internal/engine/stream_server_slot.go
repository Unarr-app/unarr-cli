package engine

import "time"

// SlotInUse reports whether the file installed with generation gen is still the
// one /stream serves AND someone is consuming it: an open reader connection (a
// paused external player keeps its socket) or a byte of THIS file served within
// recent.
//
// A web-closed slot session uses it to tell "the browser player went away" from
// "an external player (VLC) is reading the same /stream": the web closes the
// session when the viewer hands playback to VLC, and VLC reads this very slot —
// clearing it then would cut VLC mid-film.
func (ss *StreamServer) SlotInUse(gen uint64, recent time.Duration) bool {
	if uint64(ss.fileGeneration.Load()) != gen {
		return false
	}
	if ss.ActiveReaders() > 0 {
		return true
	}
	if uint64(ss.servedGen.Load()) != gen {
		return false // nothing of this file was ever served
	}
	return time.Since(time.Unix(0, ss.servedUnixNano.Load())) < recent
}

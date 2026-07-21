package main

// "System default" player — the layer that needs no list at all.
//
// The dialect table only knows players someone added by name, and LookPath
// only sees binaries on PATH: a Flatpak/Snap/AppImage player is invisible to
// both, however prominently it is set as the user's default video player. So
// before giving up (and before falling back to the browser), ask the OS who
// opens video and launch that.
//
// The trade-off is deliberate: we hand over a URL and nothing else. Resume,
// title and language preferences need a known flag dialect, so a system
// launch plays from the start. Better than the previous behaviour, which was
// "no player found" on a machine that has one.
//
// IMPORTANT (why not just xdg-open/open with the URL): the stream is an http
// URL, and every OS resolves http by SCHEME — that is the browser, not the
// video player. The per-OS files resolve the handler for a video CONTENT TYPE
// instead, and launch that app with the URL.

import (
	"fmt"
	"os"
)

// systemPlayerArgv returns the argv that opens url in the OS default video
// player, or ok=false when no such handler can be determined. Implemented per
// OS in playersystem_{linux,darwin,windows}.go.
//
// Test seam: swapped in tests so the resolution can be faked without a desktop.
var systemPlayerArgv = func(url string) ([]string, bool) {
	argv, err := defaultVideoPlayerArgv(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "unarr-desktop: system player:", err)
		return nil, false
	}
	return argv, len(argv) > 0
}

// videoProbeTypes are the content types asked about, in order. A machine that
// plays video at all answers for at least one of them; mkv leads because it is
// what unarr streams most.
var videoProbeTypes = []string{"video/x-matroska", "video/mp4"}

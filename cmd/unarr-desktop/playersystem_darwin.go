package main

// macOS backend for the system-default player: LaunchServices.
//
// There is no non-cgo API for "who opens video", and `open <url>` would
// resolve the http SCHEME (the browser). What there IS, is the preferences
// file LaunchServices writes whenever the user sets a default app — read it,
// find the handler for a movie content type, and `open -b <bundle id>`.
//
// A machine where the user never changed the default has no entry, and we
// report that rather than guessing: the factory default is QuickTime, which
// handles neither Matroska nor a long-lived http stream well. No handler →
// the caller moves on to the web player, which always works.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// launchServicesPlist holds the user's role-handler assignments.
const launchServicesPlist = "Library/Preferences/com.apple.LaunchServices/com.apple.launchservices.secure.plist"

// movieUTIs are the content types a video app registers for, most specific
// first. matroska-video is what mkv maps to; public.movie is the umbrella an
// app claiming "all video" registers.
var movieUTIs = []string{"org.matroska.mkv", "public.movie", "public.mpeg-4"}

type lsHandler struct {
	ContentType string `json:"LSHandlerContentType"`
	RoleAll     string `json:"LSHandlerRoleAll"`
	RoleViewer  string `json:"LSHandlerRoleViewer"`
}

// defaultVideoPlayerArgv resolves the bundle id registered to view movies and
// returns the `open -b` argv for it.
func defaultVideoPlayerArgv(url string) ([]string, error) {
	bundleID, err := defaultMovieBundleID()
	if err != nil {
		return nil, err
	}
	openBin, err := lookPath("open")
	if err != nil {
		return nil, fmt.Errorf("open(1) not found")
	}
	// -b targets the bundle id; the URL after `--` can never be read as a flag.
	return []string{openBin, "-b", bundleID, "--", url}, nil
}

// defaultMovieBundleID reads LaunchServices' preferences and returns the
// bundle id the user assigned to a movie type.
func defaultMovieBundleID() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home directory: %w", err)
	}
	plist := filepath.Join(home, launchServicesPlist)
	if _, statErr := statFile(plist); statErr != nil {
		return "", fmt.Errorf("no LaunchServices preferences yet")
	}
	plutil, err := lookPath("plutil")
	if err != nil {
		return "", fmt.Errorf("plutil not found")
	}
	// The file is a binary plist; plutil is the only decoder guaranteed present.
	out, err := exec.Command(plutil, "-convert", "json", "-o", "-", plist).Output()
	if err != nil {
		return "", fmt.Errorf("read LaunchServices preferences: %w", err)
	}
	var prefs struct {
		Handlers []lsHandler `json:"LSHandlers"`
	}
	if err := json.Unmarshal(out, &prefs); err != nil {
		return "", fmt.Errorf("parse LaunchServices preferences: %w", err)
	}
	return pickMovieHandler(prefs.Handlers)
}

// pickMovieHandler returns the first assignment matching a movie UTI, in
// movieUTIs order — a handler set for mkv specifically beats a generic
// "all movies" one.
func pickMovieHandler(handlers []lsHandler) (string, error) {
	for _, uti := range movieUTIs {
		for _, h := range handlers {
			if !strings.EqualFold(h.ContentType, uti) {
				continue
			}
			if id := firstNonEmpty(h.RoleAll, h.RoleViewer); id != "" {
				return id, nil
			}
		}
	}
	return "", fmt.Errorf("no default application registered for video")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

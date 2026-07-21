package main

// Windows backend for the system-default player: the file association chain.
//
//	HKCU\…\FileExts\.mkv\UserChoice\ProgId   (what the user picked)
//	→ else HKCR\.mkv\(Default)               (what the installer registered)
//	→ HKCR\<ProgId>\shell\open\command       (the command line)
//
// `start <url>` is not an option: cmd resolves the http scheme, i.e. the
// browser. The registered command line is the only place the actual video
// player is named.

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// videoExtensions are probed in order; mkv is what unarr streams most.
var videoExtensions = []string{".mkv", ".mp4"}

// defaultVideoPlayerArgv resolves the registered "open" command for a video
// extension and substitutes the URL for its file placeholder.
func defaultVideoPlayerArgv(url string) ([]string, error) {
	for _, ext := range videoExtensions {
		progID, ok := progIDFor(ext)
		if !ok {
			continue
		}
		command, ok := openCommandFor(progID)
		if !ok {
			continue
		}
		argv := expandWindowsCommand(command, url)
		if len(argv) > 0 {
			return argv, nil
		}
	}
	return nil, fmt.Errorf("no default application registered for video")
}

// progIDFor returns the ProgId associated with an extension: the user's
// explicit choice first, then the machine-wide registration.
func progIDFor(ext string) (string, bool) {
	userChoice := `Software\Microsoft\Windows\CurrentVersion\Explorer\FileExts\` + ext + `\UserChoice`
	if id, ok := regString(registry.CURRENT_USER, userChoice, "ProgId"); ok {
		return id, true
	}
	return regString(registry.CLASSES_ROOT, ext, "")
}

// openCommandFor returns the raw command line registered for a ProgId's
// "open" verb, e.g. `"C:\Program Files\VideoLAN\VLC\vlc.exe" --started-from-file "%1"`.
func openCommandFor(progID string) (string, bool) {
	return regString(registry.CLASSES_ROOT, progID+`\shell\open\command`, "")
}

// regString reads one string value (name "" = the key's default value).
// Any failure — missing key, wrong type, no permission — reads as "not set".
func regString(root registry.Key, path, name string) (string, bool) {
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if err != nil {
		return "", false
	}
	defer k.Close()
	v, _, err := k.GetStringValue(name)
	if err != nil || strings.TrimSpace(v) == "" {
		return "", false
	}
	return strings.TrimSpace(v), true
}

// expandWindowsCommand tokenizes a registered command line and replaces the
// file placeholder with the URL. Placeholders that expand to shell/DDE
// metadata (%*, %L, %V, %1) are all handled; a command mentioning none gets
// the URL appended, so the player still receives something to play.
func expandWindowsCommand(command, url string) []string {
	var argv []string
	replaced := false
	for _, tok := range splitCommand(command) {
		switch {
		case strings.Contains(tok, "%1"), strings.Contains(tok, "%L"),
			strings.Contains(tok, "%l"), strings.Contains(tok, "%V"),
			strings.Contains(tok, "%v"), strings.Contains(tok, "%U"),
			strings.Contains(tok, "%u"):
			// Substitute INSIDE the token: a placeholder is sometimes embedded
			// in a larger argument, and the URL must never split into its own.
			for _, ph := range []string{"%1", "%L", "%l", "%V", "%v", "%U", "%u"} {
				tok = strings.ReplaceAll(tok, ph, url)
			}
			replaced = true
			argv = append(argv, tok)
		case tok == "%*":
			continue // "all remaining switches" — nothing to pass on
		default:
			argv = append(argv, tok)
		}
	}
	if len(argv) == 0 {
		return nil
	}
	if !replaced {
		argv = append(argv, url)
	}
	return argv
}

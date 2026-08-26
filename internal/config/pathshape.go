package config

import (
	"fmt"
	"strings"
)

// pathShapeError rejects a configured directory that the target OS can never
// create, whatever the permissions or the disks say. It runs before any
// filesystem call, so the user gets "your config is wrong" instead of a syscall
// error blamed on the environment.
//
// It exists because of a real crash report: a Windows agent with
// downloads.dir = `D:\D:\` failed every start with
//
//	create download dir: mkdir D:\D:\: The filename, directory name, or volume
//	label syntax is incorrect.
//
// twelve times in a row, reported as a PERMISSION problem (see cmd.runDaemon),
// which is the one thing it was not — no amount of Run-as-Administrator fixes a
// second drive letter in the middle of a path.
//
// Deliberately minimal: a drive-letter colon outside the leading volume name is
// the only shape rejected here. Windows forbids more characters than that
// (<>"|?*), but a validator that guesses is worse than one that is certain —
// it would start refusing paths that work.
func pathShapeError(label, dir, goos string) error {
	if goos != "windows" {
		return nil // ':' is a legal filename byte everywhere else
	}
	if rest, ok := afterWindowsVolume(dir); ok && strings.Contains(rest, ":") {
		// %s, not %q: this message is read almost exclusively on Windows, where
		// %q turns `D:\Media` into "D:\\Media" and the user has to mentally
		// un-escape their own path before they can fix it.
		return fmt.Errorf("%s: %s is not a valid Windows path — a drive letter may only appear at the start", label, dir)
	}
	return nil
}

// afterWindowsVolume returns the part of p that follows its leading volume
// name, and whether p is a shape this check understands.
//
// UNC and extended-length prefixes (`\\server\share`, `\\?\C:\…`) return false:
// they carry colons of their own by construction, and rejecting those would
// break the very users who type a path correctly.
func afterWindowsVolume(p string) (string, bool) {
	if strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//") {
		return "", false
	}
	if len(p) >= 2 && p[1] == ':' && isDriveLetter(p[0]) {
		return p[2:], true
	}
	return p, true
}

func isDriveLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

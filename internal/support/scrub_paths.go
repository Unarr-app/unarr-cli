package support

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// homeMarker is what a home directory becomes. `~` is the shape every reader
// already parses, so "~/Media" still reads as a path and still tells a
// developer the library lives under the home directory — which is the actual
// diagnostic. The username was never part of it.
const homeMarker = "~"

// minHomeLen guards against rewriting the world. A home directory reported as
// "/" or "C:" is not a home directory, and substituting it would turn every
// absolute path in the bundle into "~" and destroy the evidence. Anything that
// short is treated as "no home to erase".
const minHomeLen = 4

// homeRewrites returns the substitutions that erase the account name from
// on-disk paths.
//
// This exists because the config projection and the free-text sections
// disagreed. config.redacted.toml reduces downloads.dir to "<set>", but
// doctor.json quoted the same directory in full — and on a real machine that
// full path reads /home/<name>, /Users/<name> or C:\Users\<Name>. Every bundle
// carried the account holder's name in a file the user is told to attach to a
// public issue, while the file next to it went to some trouble to hide it.
//
// Only the home PREFIX is rewritten, never the bare username on its own. A
// username can be short and ordinary ("max", "root", "media"), and a scrubber
// that erased it wherever it appeared would rewrite the middle of file names
// and log messages — corrupting the evidence the bundle exists to carry. Same
// reasoning as minLiteralLen: the prefix is the vector that actually leaks, and
// it is the one that can be matched exactly.
func homeRewrites() []rewrite {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	home = strings.TrimRight(filepath.Clean(home), `/\`)
	if len(home) < minHomeLen {
		return nil
	}

	// Windows text mixes separators freely: the Go code writes "\", a URL or a
	// JSON dump writes "/", and both land in the same bundle.
	variants := []string{home}
	if runtime.GOOS == "windows" {
		if alt := strings.ReplaceAll(home, `\`, "/"); alt != home {
			variants = append(variants, alt)
		}
	}

	out := make([]rewrite, 0, len(variants))
	for _, v := range variants {
		out = append(out, rewrite{re: homePattern(v), to: homeMarker})
	}
	return out
}

// homePattern matches a home directory only at a word boundary, so
// /home/anna does not match inside /home/annabel — a real path belonging to a
// different account, which the crude prefix replace would have mangled into
// "~bel" and quietly misreported.
//
// Windows paths are matched case-insensitively: NTFS is, and the same directory
// is written C:\Users\Anna by the shell and c:\users\anna by half the APIs.
func homePattern(home string) *regexp.Regexp {
	pat := regexp.QuoteMeta(home)
	// \b only means anything after a word character. A home ending in a
	// separator or a colon has an unambiguous end already.
	if last := home[len(home)-1]; isWordByte(last) {
		pat += `\b`
	}
	if runtime.GOOS == "windows" {
		pat = "(?i)" + pat
	}
	return regexp.MustCompile(pat)
}

func isWordByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}

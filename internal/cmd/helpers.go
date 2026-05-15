package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// openBrowser opens a URL in the default browser.
//
// The URL is restricted to http(s) so that a hostile caller cannot trick
// xdg-open/open into interpreting it as a flag (a leading "-" would otherwise
// match a switch on every helper we shell out to). Where the helper supports
// it we also append "--" to terminate switch parsing as belt-and-braces.
func openBrowser(url string) {
	if !isSafeBrowserURL(url) {
		return
	}
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", "--", url)
	case "windows":
		// rundll32 does not parse switches from positional args.
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default: // linux, freebsd
		c = exec.Command("xdg-open", url)
	}
	_ = c.Start() // fire and forget; best-effort
}

// isSafeBrowserURL accepts only http(s) URLs. Other schemes (file://, javascript:,
// data:, ...) and flag-shaped strings ("--help") are rejected.
func isSafeBrowserURL(url string) bool {
	return strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")
}

// defaultDownloadDir returns a sensible default download directory.
func defaultDownloadDir() string {
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, "Media"),
		filepath.Join(home, "Downloads", "unarr"),
	}
	for _, d := range candidates {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			return d
		}
	}
	return filepath.Join(home, "Media")
}

// expandHome expands a leading ~/ to the user's home directory.
func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return path
}

// isTerminal checks if stdin is a terminal (not piped).
func isTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

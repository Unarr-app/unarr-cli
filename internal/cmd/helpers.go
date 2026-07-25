package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

// openBrowser opens a URL in the default browser.
//
// The URL is restricted to http(s) so that a hostile caller cannot trick
// xdg-open/open into interpreting it as a flag (a leading "-" would otherwise
// match a switch on every helper we shell out to). Where the helper supports
// it we also append "--" to terminate switch parsing as belt-and-braces.
//
// The error says only that the helper could not be LAUNCHED (no xdg-open, no
// session to open into over SSH/Docker/WSL); a nil error is not a promise that
// a window appeared. Callers must print the URL either way.
func openBrowser(url string) error {
	if !isSafeBrowserURL(url) {
		return fmt.Errorf("refusing to open non-http(s) URL %q", url)
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
	return c.Start()
}

// isSafeBrowserURL accepts only http(s) URLs. Other schemes (file://, javascript:,
// data:, ...) and flag-shaped strings ("--help") are rejected.
func isSafeBrowserURL(url string) bool {
	return strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")
}

// defaultAPIURL is the server used when neither config.toml nor a flag names
// one. Read from config.Default() so the wizard, `login`, `up` and the help
// text can never drift from the value a fresh config is actually written with.
func defaultAPIURL() string {
	return config.Default().Auth.APIURL
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

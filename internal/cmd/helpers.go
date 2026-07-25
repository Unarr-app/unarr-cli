package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/agent"
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

// setupHint names the command that can ACTUALLY finish setup on this machine.
//
// "run unarr init" is a dead end wherever the wizard cannot run: in a container
// there is no tty and no browser, and telling the user to exec into it to answer
// prompts is worse than the one thing that does work there — an auth-key from
// the web, handed to the agent as an environment variable. Same for any other
// non-interactive host (systemd unit, provisioning script, CI).
func setupHint(apiURL string) string {
	return setupHintFor(apiURL, agent.RunningInDocker(), isTerminal())
}

// setupHintFor is the pure form of setupHint, with the two environment probes
// passed in so every branch can be pinned by a test (isTerminal() is true even
// under `go test`, whose stdin is the character device /dev/null).
func setupHintFor(apiURL string, inDocker, interactive bool) string {
	if apiURL == "" {
		apiURL = defaultAPIURL()
	}
	where := "get a one-time key at " + apiURL + "/profile?tab=agents"
	if inDocker {
		return "recreate the container with -e UNARR_AUTHKEY=… (" + where + ")"
	}
	if !interactive {
		return "run `unarr up --auth-key=…` (" + where + ")"
	}
	return "run `unarr init`"
}

// sudoGuard aborts a command that writes USER-level state — config.toml, the
// systemd user unit, the download directory — when the only reason we are root
// is a `sudo` in front of it. All of it would land in /root, invisible to the
// session the user actually runs the agent from.
func sudoGuard(command string) error {
	if !runningUnderSudo() {
		return nil
	}
	return fmt.Errorf("don't run this with sudo — config, the user service and downloads would land in /root\n"+
		"  Run it as your normal user: unarr %s", command)
}

// runningUnderSudo reports whether this process is root *because a normal user
// typed sudo*, as opposed to a legitimately root-only environment (Docker, a NAS
// shell, a root-owned systemd unit) where root is the only user there is and
// everything must keep working.
func runningUnderSudo() bool {
	return isSudoEnv(os.Geteuid(), os.Getenv("SUDO_USER"), os.Getenv("SUDO_UID"), os.Getenv("HOME"), loginName())
}

// isSudoEnv is the pure predicate behind runningUnderSudo, split out so the
// sudo / sudo -i / su - / container matrix can be pinned by tests.
//
// SUDO_USER catches plain `sudo unarr …`. `sudo -i` and `sudo su -` start a
// login shell that scrubs SUDO_USER; what survives is SUDO_UID (kept by
// `sudo -i`) or, failing that, the mismatch between the root HOME they hand us
// and the user who actually owns the login session. A genuine root environment
// has no login name at all (no utmp entry), so it falls through to false.
func isSudoEnv(euid int, sudoUser, sudoUID, home, login string) bool {
	if euid != 0 {
		return false
	}
	if sudoUser != "" || sudoUID != "" {
		return true
	}
	return home == "/root" && login != "" && login != "root"
}

// loginName is the user owning the controlling terminal. su/sudo do not change
// it, which is what makes it the "who really logged in" signal. Empty when there
// is no tty or no utmp entry — containers and systemd units, exactly the cases
// that must NOT be read as sudo.
func loginName() string {
	out, err := exec.Command("logname").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
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

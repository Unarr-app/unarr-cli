package engine

import (
	"fmt"
	"os/exec"
	"runtime"
)

// OpenPlayer attempts to open a media player with the given stream URL.
// Returns the player name and the running command.
// If override is set, it uses that command directly.
func OpenPlayer(url, override string) (string, *exec.Cmd, error) {
	if override != "" {
		cmd := exec.Command(override, url)
		if err := cmd.Start(); err != nil {
			return override, nil, fmt.Errorf("start %s: %w", override, err)
		}
		return override, cmd, nil
	}

	// Try mpv first (best streaming support)
	if path, err := exec.LookPath("mpv"); err == nil {
		cmd := exec.Command(path, "--no-terminal", url)
		if err := cmd.Start(); err == nil {
			return "mpv", cmd, nil
		}
	}

	// Try VLC
	if path, err := exec.LookPath("vlc"); err == nil {
		cmd := exec.Command(path, url)
		if err := cmd.Start(); err == nil {
			return "vlc", cmd, nil
		}
	}

	// Try cvlc (VLC headless)
	if path, err := exec.LookPath("cvlc"); err == nil {
		cmd := exec.Command(path, url)
		if err := cmd.Start(); err == nil {
			return "vlc (headless)", cmd, nil
		}
	}

	// Browser fallback
	name, cmd, err := openBrowser(url)
	if err != nil {
		return "", nil, fmt.Errorf("no player found: install mpv or vlc, or open %s manually", url)
	}
	return name, cmd, nil
}

func openBrowser(url string) (string, *exec.Cmd, error) {
	switch runtime.GOOS {
	case "linux":
		if path, err := exec.LookPath("xdg-open"); err == nil {
			cmd := exec.Command(path, url)
			if err := cmd.Start(); err == nil {
				return "browser", cmd, nil
			}
		}
	case "darwin":
		cmd := exec.Command("/usr/bin/open", url)
		if err := cmd.Start(); err == nil {
			return "browser", cmd, nil
		}
	case "windows":
		cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
		if err := cmd.Start(); err == nil {
			return "browser", cmd, nil
		}
	}
	return "", nil, fmt.Errorf("no browser opener found")
}

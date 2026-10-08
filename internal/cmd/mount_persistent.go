package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/service"
)

// The installed service always reads the default config and does not inherit
// this shell's overrides. Refuse ambiguity before login, saving or service calls.
func persistentMountConfig() error {
	keys := []string{"UNARR_CONFIG_DIR", "UNARR_API_KEY", "UNARR_API_URL", "UNARR_DOWNLOAD_DIR", "UNARR_COUNTRY", "UNARR_TELEMETRY"}
	if runtime.GOOS == "linux" {
		keys = append(keys, "XDG_CONFIG_HOME", "XDG_DATA_HOME")
	}
	for _, key := range keys {
		if os.Getenv(key) != "" {
			return fmt.Errorf("persistent mount uses the default saved configuration; unset %s first (mount serve supports custom configuration)", key)
		}
	}
	selected, err := filepath.Abs(resolvedConfigPath())
	if err != nil {
		return err
	}
	standard, err := filepath.Abs(config.FilePath())
	if err != nil {
		return err
	}
	if !sameMountConfigPath(selected, standard) {
		return errors.New("persistent mount and umount require the default config; use mount serve --config for a custom config")
	}
	return nil
}

func sameMountConfigPath(selected, standard string) bool {
	if real, err := filepath.EvalSymlinks(selected); err == nil {
		selected = real
	}
	if real, err := filepath.EvalSymlinks(standard); err == nil {
		standard = real
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(selected, standard)
	}
	return selected == standard
}

func mountDaemonPreflight(cfg config.Config) error {
	if errCfgLoad != nil {
		return fmt.Errorf("read agent config: %w; run unarr init", errCfgLoad)
	}
	if cfg.Download.Dir == "" {
		return errors.New("the background agent requires a download directory; run unarr init before mounting")
	}
	if err := cfg.ValidatePaths(); err != nil {
		return fmt.Errorf("agent configuration: %w; run unarr config", err)
	}
	return nil
}

func mountServiceInstalled() bool {
	return service.Respawns() || (runtime.GOOS == "windows" && windowsTaskInstalled())
}

// Artifact presence means installed, not running. Stop and parked intent always
// wins so disabling the optional mount cannot resurrect an intentionally idle agent.
func mountServiceActive() (bool, error) {
	if parkedMarkerExists() || agent.StopIntentExists() {
		return false, nil
	}
	switch runtime.GOOS {
	case "linux":
		out, err := svcOutput("systemctl", "--user", "is-active", service.SystemdUnitName)
		if out == "active" {
			return true, nil
		}
		if out == "inactive" || out == "failed" || out == "unknown" {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("query agent state: %w: %s", err, out)
		}
		return false, nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return false, err
		}
		a, err := newLaunchdAgent(home)
		if err != nil {
			return false, err
		}
		out, _, err := a.status()
		return launchdPID(out) > 0, err
	default:
		state := agent.ReadState()
		return isDaemonAlive(state), nil
	}
}

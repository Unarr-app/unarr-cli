package config

import (
	"fmt"
	"net"
	"path/filepath"
	"runtime"
	"time"
)

// MountConfig holds device-local settings only; accounts are managed on the web.
type MountConfig struct {
	Enabled         bool   `toml:"enabled"`
	Directory       string `toml:"directory,omitempty"`
	Listen          string `toml:"listen,omitempty"`
	CacheDir        string `toml:"cache_dir,omitempty"`
	NZBDir          string `toml:"nzb_dir,omitempty"`
	RefreshInterval string `toml:"refresh_interval,omitempty"`
}

// NZBDirectory returns the inbox watched by the remote mount. An explicit
// nzb_dir remains an override, while the default keeps web-selected manifests
// beside the active config so the daemon and `unarr mount` share one location.
func (m MountConfig) NZBDirectory(configPath string) string {
	if m.NZBDir != "" {
		return m.NZBDir
	}
	return filepath.Join(filepath.Dir(configPath), "mount-nzbs")
}

func (m MountConfig) Address() string {
	if m.Listen == "" {
		return "127.0.0.1:11820"
	}
	return m.Listen
}
func (m MountConfig) RefreshEvery() time.Duration {
	d, err := time.ParseDuration(m.RefreshInterval)
	if err != nil || d <= 0 {
		return time.Minute
	}
	return d
}
func (m MountConfig) Validate() error {
	if !m.Enabled {
		return nil
	}
	host, port, err := net.SplitHostPort(m.Address())
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return fmt.Errorf("mount.listen must be a loopback IP:port")
	}
	if _, err := net.LookupPort("tcp", port); err != nil {
		return fmt.Errorf("invalid mount.listen port")
	}
	if m.RefreshInterval != "" {
		d, err := time.ParseDuration(m.RefreshInterval)
		if err != nil || d < 10*time.Second {
			return fmt.Errorf("mount.refresh_interval must be at least 10s")
		}
	}
	return m.validatePaths()
}

func (m MountConfig) validatePaths() error {
	if m.CacheDir != "" && !filepath.IsAbs(m.CacheDir) {
		return fmt.Errorf("mount.cache_dir must be absolute")
	}
	if m.Directory != "" && !filepath.IsAbs(m.Directory) && !(runtime.GOOS == "windows" && IsWindowsMountDrive(m.Directory)) {
		return fmt.Errorf("mount.directory must be absolute")
	}
	if m.NZBDir != "" && !filepath.IsAbs(m.NZBDir) {
		return fmt.Errorf("mount.nzb_dir must be absolute")
	}
	return nil
}

// IsWindowsMountDrive accepts only a bare ASCII drive letter. A drive-relative
// path such as X:media must never be expanded using the process working directory.
func IsWindowsMountDrive(directory string) bool {
	return len(directory) == 2 && directory[1] == ':' &&
		((directory[0] >= 'A' && directory[0] <= 'Z') || (directory[0] >= 'a' && directory[0] <= 'z'))
}

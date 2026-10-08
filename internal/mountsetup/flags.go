package mountsetup

import (
	"os"
	"runtime"
)

// MountFlags is shared by the capability probe and the actual mount. Older
// installations must understand the options we use, not merely have a mount verb.
func MountFlags() []string {
	flags := []string{
		"--config", os.DevNull, "--read-only", "--vfs-cache-mode", "off",
		"--buffer-size", "4M", "--vfs-read-chunk-size", "32M",
		"--vfs-read-chunk-size-limit", "128M", "--dir-cache-time", "15s",
		"--poll-interval", "0", "--webdav-pacer-min-sleep", "0",
		"--low-level-retries", "2", "--retries", "2",
	}
	if runtime.GOOS == "windows" {
		// WinFsp >=1.9: restrict owner rights as well as inherited grants, so
		// Windows cannot acknowledge deletes on the read-only remote mount.
		flags = append(flags, "-o", "FileSecurity=D:P(A;;FRFX;;;OW)")
	}
	return flags
}

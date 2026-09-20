package mountsetup

import "os"

// MountFlags is shared by the capability probe and the actual mount. Older
// installations must understand the options we use, not merely have a mount verb.
func MountFlags() []string {
	return []string{
		"--config", os.DevNull, "--read-only", "--vfs-cache-mode", "off",
		"--buffer-size", "4M", "--vfs-read-chunk-size", "32M",
		"--vfs-read-chunk-size-limit", "128M", "--dir-cache-time", "15s",
		"--poll-interval", "0", "--webdav-pacer-min-sleep", "0",
		"--low-level-retries", "2", "--retries", "2",
	}
}

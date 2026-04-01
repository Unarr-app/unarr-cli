package agent

import (
	"io/fs"
	"path/filepath"
)

// DirSize returns the total size in bytes of all files under dir.
func DirSize(dir string) (int64, error) {
	var size int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if !d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return nil
			}
			size += info.Size()
		}
		return nil
	})
	return size, err
}

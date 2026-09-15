package postprocess

import (
	"os"
	"path/filepath"
	"strings"
)

// isRolledOverVolume reports whether name is a .t00-.z99 volume of a large
// old-style RAR set (".r00"-".r99" rolls over to ".s00", then ".t00" …). names
// holds the directory's lower-cased file names.
//
// The extension alone is not enough: .t64, .v64 and .z64 are tape and ROM
// images, and may be the very payload just extracted into this directory. So a
// volume counts only when the previous letter of the same set filled up to 99
// beside it (".s99" before any ".tNN").
func isRolledOverVolume(name string, names map[string]bool) bool {
	ext := strings.ToLower(filepath.Ext(name))
	if len(ext) != 4 || ext[1] < 't' || ext[1] > 'z' || !isNumeric(ext[2:]) {
		return false
	}
	stem := strings.ToLower(name[:len(name)-len(ext)])
	return names[stem+"."+string(ext[1]-1)+"99"]
}

// lowerNames returns the lower-cased names of dir's entries, the lookup set
// isRolledOverVolume expects. An unreadable dir yields an empty set.
func lowerNames(dir string) map[string]bool {
	entries, _ := os.ReadDir(dir)
	names := make(map[string]bool, len(entries))
	for _, e := range entries {
		names[strings.ToLower(e.Name())] = true
	}
	return names
}

// isRolledOverVolumeOf is isRolledOverVolume restricted to the classic set whose
// archiveStem key is stem, so cleanup never reaches into another set.
func isRolledOverVolumeOf(name, stem string, names map[string]bool) bool {
	return stem == name[:len(name)-len(filepath.Ext(name))]+"|rar" && isRolledOverVolume(name, names)
}

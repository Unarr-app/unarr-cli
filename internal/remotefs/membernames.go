package remotefs

import (
	"fmt"
	"path"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nzb"
)

// Allocate all manifest names before probing. A missing article cannot move a
// healthy sibling's name on recovery. Reserve directories first so a file can
// never occupy the path needed by another member's parent directory.
func memberPaths(files []nzb.File, title, id string) []string {
	paths := make([]string, len(files))
	dirs := make(map[string]string)
	for i, f := range files {
		parts := strings.Split(releasePath(title, id, f.Filename()), "/")
		for j := 1; j < len(parts); j++ {
			p := strings.Join(parts[:j], "/")
			key := strings.ToLower(p)
			if previous, ok := dirs[key]; ok {
				parts[j-1] = path.Base(previous)
			} else {
				dirs[key] = p
			}
		}
		paths[i] = strings.Join(parts, "/")
	}
	claimed := make(map[string]bool, len(files))
	for i, original := range paths {
		candidate := original
		for suffix := 1; ; suffix++ {
			key := strings.ToLower(candidate)
			_, directory := dirs[key]
			if !directory && !claimed[key] {
				claimed[key] = true
				paths[i] = candidate
				break
			}
			ext := path.Ext(original)
			leaf := strings.TrimSuffix(path.Base(original), ext)
			candidate = path.Join(path.Dir(original), fmt.Sprintf("%s [file-%d-%d]%s", leaf, i+1, suffix, ext))
		}
	}
	return paths
}

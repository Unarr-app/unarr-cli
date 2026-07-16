// Package davfs builds a read-only, organized virtual filesystem over the
// agent's scanned library cache (internal/library) and exposes it through the
// golang.org/x/net/webdav FileSystem interface. The tree is synthetic —
// /Movies/<Title (Year)>/<file> and /TV Shows/<Show>/Season NN/<file> — while
// leaves point at the real on-disk media so a GET streams the actual file with
// native HTTP Range support. It is READ-ONLY: every mutating operation returns
// os.ErrPermission (see davfs.go), so PUT/DELETE/MKCOL/COPY/MOVE never touch
// disk.
//
// Phase 2 (deferred, out of scope): a future leaf kind could 302-redirect to a
// signed debrid URL instead of holding a realPath. The node model leaves room
// for that without reworking the tree.
package davfs

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/library"
)

// Top-level virtual directories.
const (
	movieRoot = "Movies"
	tvRoot    = "TV Shows"
)

// node is one entry in the virtual tree. Directories are synthetic (children +
// order); files carry realPath so a GET can os.Open the real media. A node is
// treated as IMMUTABLE once buildTree/finalize returns — the FS only ever swaps
// the whole root pointer under lock, never mutates a published subtree — so
// readers may hold a *node after releasing the FS lock.
type node struct {
	name     string
	isDir    bool
	modTime  time.Time
	size     int64
	realPath string           // files only
	children map[string]*node // dirs only
	order    []string         // dirs only: child names, sorted, for stable Readdir
}

func newDir(name string) *node {
	return &node{name: name, isDir: true, children: map[string]*node{}}
}

// excludeNamePatterns mirrors library/scanner.go's excludePatterns so a
// sample/trailer/extra that somehow reached the cache is still hidden from the
// WebDAV view (defense-in-depth — the scanner already skips these).
var excludeNamePatterns = []string{
	"sample", "trailer", "featurette", "extras", "bonus",
	"behind the scenes", "deleted scenes", "interview",
}

// episodeMarker locates the SxxEyy / NxNN token inside a parsed title so the
// SHOW folder uses only the series part. library's CleanTitle leaves the
// episode marker in Title (e.g. "Show Name S01E09 …"), which would otherwise
// give every episode its own folder — same problem library/skipdetect.go
// solves with its own marker regex.
var episodeMarker = regexp.MustCompile(`(?i)\b(S\d{1,2}\s*E\d{1,4}|\d{1,2}x\d{2})\b`)

// buildTree constructs the organized virtual root from the scanned items.
// Hidden items (scan errors, damaged files, samples) are skipped.
func buildTree(items []library.LibraryItem) *node {
	root := newDir("")
	for i := range items {
		item := items[i]
		if hidden(item) {
			continue
		}
		dirSegs, leaf := virtualPath(item)
		if leaf == "" {
			continue
		}
		dir := mkdirs(root, dirSegs)
		addLeaf(dir, leaf, item)
	}
	finalize(root)
	return root
}

// hidden reports whether an item must be kept out of the library view:
// unscannable (missing path / scan error) or a damaged/incomplete download, or
// a sample/trailer/extra file.
func hidden(item library.LibraryItem) bool {
	if item.FilePath == "" || item.ScanError != "" {
		return true
	}
	if mi := item.MediaInfo; mi != nil && mi.Integrity != nil && mi.Integrity.Damaged {
		return true
	}
	return isExcludedName(item.FileName)
}

func isExcludedName(name string) bool {
	lower := strings.ToLower(name)
	for _, p := range excludeNamePatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// virtualPath returns the directory segments and leaf filename for an item.
// A movie lands under /Movies/<Title (Year)>; a show under
// /TV Shows/<Show>/Season NN (or /Specials for season 0/unknown).
func virtualPath(item library.LibraryItem) ([]string, string) {
	leaf := sanitizeName(item.FileName)
	if leaf == "" {
		return nil, ""
	}
	if library.DeriveContentType(item) == "show" {
		return []string{tvRoot, showFolder(item), seasonFolder(item.Season)}, leaf
	}
	return []string{movieRoot, movieFolder(item)}, leaf
}

func movieFolder(item library.LibraryItem) string {
	name := sanitizeName(item.Title)
	if name == "" {
		name = "Unknown"
	}
	if item.Year != "" {
		name += " (" + item.Year + ")"
	}
	return name
}

func showFolder(item library.LibraryItem) string {
	name := sanitizeName(showTitle(item))
	if name == "" {
		return "Unknown Show"
	}
	return name
}

// showTitle strips the episode marker from a parsed title to recover the series
// name used as the show folder.
func showTitle(item library.LibraryItem) string {
	t := strings.TrimSpace(item.Title)
	if loc := episodeMarker.FindStringIndex(t); loc != nil {
		t = strings.TrimSpace(t[:loc[0]])
	}
	return t
}

func seasonFolder(season int) string {
	if season <= 0 {
		return "Specials"
	}
	return fmt.Sprintf("Season %02d", season)
}

// mkdirs walks (creating as needed) the directory chain under root and returns
// the leaf directory node.
func mkdirs(root *node, segs []string) *node {
	n := root
	for _, seg := range segs {
		child, ok := n.children[seg]
		if !ok {
			child = newDir(seg)
			n.children[seg] = child
		}
		n = child
	}
	return n
}

// addLeaf inserts a file node, suffixing " (2)", " (3)" … before the extension
// on a duplicate leaf name within the same directory.
func addLeaf(dir *node, leaf string, item library.LibraryItem) {
	name := uniqueLeafName(dir, leaf)
	dir.children[name] = &node{
		name:     name,
		size:     item.FileSize,
		modTime:  parseModTime(item.ModTime),
		realPath: item.FilePath,
	}
}

func uniqueLeafName(dir *node, leaf string) string {
	if _, exists := dir.children[leaf]; !exists {
		return leaf
	}
	ext := filepath.Ext(leaf)
	base := strings.TrimSuffix(leaf, ext)
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, exists := dir.children[cand]; !exists {
			return cand
		}
	}
}

// finalize sorts every directory's children for stable Readdir order and sets
// each directory's modTime to its newest descendant (stable ETags — webdav keys
// on ModTime+Size).
func finalize(n *node) {
	if !n.isDir {
		return
	}
	n.order = make([]string, 0, len(n.children))
	for name := range n.children {
		n.order = append(n.order, name)
	}
	sort.Strings(n.order)
	var newest time.Time
	for _, name := range n.order {
		c := n.children[name]
		finalize(c)
		if c.modTime.After(newest) {
			newest = c.modTime
		}
	}
	n.modTime = newest
}

// sanitizeName makes a title/filename safe as a single virtual path segment:
// path separators become spaces, control characters are dropped, internal
// whitespace is collapsed, and stray leading/trailing dots and spaces are
// trimmed (a trailing "." is illegal in some WebDAV clients).
func sanitizeName(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\':
			return ' '
		case r < 0x20 || r == 0x7f:
			return -1
		default:
			return r
		}
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	return strings.Trim(s, ". ")
}

func parseModTime(s string) time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

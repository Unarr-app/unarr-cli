package davfs

import (
	"context"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/webdav"

	"github.com/Unarr-app/unarr-cli/internal/library"
)

// defaultRefreshInterval bounds how often the FS re-stats the on-disk library
// cache to pick up an auto-scan's rewrite without a daemon restart.
const defaultRefreshInterval = 15 * time.Second

// writeFlags are the open flags that indicate a mutation. Any of them makes
// OpenFile reject the request (the FS is strictly read-only).
const writeFlags = os.O_WRONLY | os.O_RDWR | os.O_CREATE | os.O_TRUNC | os.O_APPEND

// Options configures a new FS.
type Options struct {
	// Load reads the library cache (typically library.LoadCache). Returning
	// (nil, nil) is treated as "no library yet" → an empty tree.
	Load func() (*library.LibraryCache, error)
	// AllowPath re-validates a leaf's real path against the allowed stream roots
	// just before it is opened (defense-in-depth vs a poisoned cache). nil skips
	// the check.
	AllowPath func(string) bool
	// CachePath is the library cache file, stat'd to detect changes.
	CachePath string
	// RefreshInterval throttles the change check; <=0 uses defaultRefreshInterval.
	RefreshInterval time.Duration
}

// FS is a read-only webdav.FileSystem backed by the library cache. The tree is
// rebuilt lazily when the cache file's mtime changes (throttled), so new
// downloads appear after the next scan without restarting the daemon.
type FS struct {
	mu           sync.RWMutex
	root         *node
	cachePath    string
	load         func() (*library.LibraryCache, error)
	allowPath    func(string) bool
	refreshEvery time.Duration
	lastStat     time.Time
	lastMod      time.Time
	loadedOnce   bool
}

// Compile-time guarantee that FS satisfies the webdav interface.
var _ webdav.FileSystem = (*FS)(nil)

// New builds an FS. It does not touch disk yet — the first Stat/OpenFile loads
// the cache.
func New(opts Options) *FS {
	every := opts.RefreshInterval
	if every <= 0 {
		every = defaultRefreshInterval
	}
	return &FS{
		root:         newDir(""),
		cachePath:    opts.CachePath,
		load:         opts.Load,
		allowPath:    opts.AllowPath,
		refreshEvery: every,
	}
}

// Stat resolves a virtual path. Directories return a synthetic FileInfo; files
// are stat'd on disk for a live size/mtime (name forced to the virtual leaf so
// a collision-suffixed name stays consistent).
func (f *FS) Stat(_ context.Context, name string) (os.FileInfo, error) {
	f.ensureFresh()
	f.mu.RLock()
	n, ok := f.resolve(name)
	f.mu.RUnlock()
	if !ok {
		return nil, os.ErrNotExist
	}
	if n.isDir {
		return n.info(), nil
	}
	return f.statFile(n)
}

func (f *FS) statFile(n *node) (os.FileInfo, error) {
	// Re-validate against the allowed roots exactly like openFileNode — without
	// this a poisoned cache leaf outside the roots would leak its existence,
	// size and mtime via PROPFIND/Stat even though a GET is refused. Report it
	// as absent (ErrNotExist) rather than forbidden so nothing is disclosed.
	if f.allowPath != nil && !f.allowPath(n.realPath) {
		log.Printf("[webdav] refusing stat outside allowed roots: %q", n.realPath)
		return nil, os.ErrNotExist
	}
	fi, err := os.Stat(n.realPath)
	if err != nil {
		return nil, err
	}
	return nodeInfo{name: n.name, size: fi.Size(), modTime: fi.ModTime()}, nil
}

// OpenFile serves reads only. Any write flag → os.ErrPermission (webdav maps
// that to 403). Directories return a synthetic listing; files are re-validated
// against the allowed roots and then os.Open'd.
func (f *FS) OpenFile(_ context.Context, name string, flag int, _ os.FileMode) (webdav.File, error) {
	if flag&writeFlags != 0 {
		return nil, os.ErrPermission
	}
	f.ensureFresh()
	f.mu.RLock()
	n, ok := f.resolve(name)
	f.mu.RUnlock()
	if !ok {
		return nil, os.ErrNotExist
	}
	if n.isDir {
		return newDirFile(n), nil
	}
	return f.openFileNode(n)
}

func (f *FS) openFileNode(n *node) (webdav.File, error) {
	if f.allowPath != nil && !f.allowPath(n.realPath) {
		log.Printf("[webdav] refusing file outside allowed roots: %q", n.realPath)
		return nil, os.ErrPermission
	}
	// realPath comes from the trusted library scan and is re-checked by AllowPath
	// above; gosec G304 is excluded project-wide (see .golangci.yml).
	fh, err := os.Open(n.realPath)
	if err != nil {
		return nil, err
	}
	return &realFile{File: fh, name: n.name}, nil
}

// Mkdir is unsupported — the FS is read-only.
func (f *FS) Mkdir(_ context.Context, _ string, _ os.FileMode) error { return os.ErrPermission }

// RemoveAll is unsupported — the FS is read-only.
func (f *FS) RemoveAll(_ context.Context, _ string) error { return os.ErrPermission }

// Rename is unsupported — the FS is read-only.
func (f *FS) Rename(_ context.Context, _, _ string) error { return os.ErrPermission }

// resolve walks the virtual tree by exact segment name. "", "/", and "." map to
// the root; any ".." segment is rejected (no upward traversal). Caller holds at
// least an RLock.
func (f *FS) resolve(name string) (*node, bool) {
	n := f.root
	if n == nil {
		return nil, false
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." {
			continue
		}
		if seg == ".." || !n.isDir {
			return nil, false
		}
		child, ok := n.children[seg]
		if !ok {
			return nil, false
		}
		n = child
	}
	return n, true
}

// ensureFresh rebuilds the tree from the cache when its mtime changed, at most
// once per refreshEvery. Never errors: a missing/broken cache keeps the last
// good tree (or an empty one), so an un-scanned agent serves an empty library
// rather than failing every request.
func (f *FS) ensureFresh() {
	f.mu.RLock()
	fresh := f.loadedOnce && time.Since(f.lastStat) < f.refreshEvery
	f.mu.RUnlock()
	if fresh {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loadedOnce && time.Since(f.lastStat) < f.refreshEvery {
		return
	}
	f.lastStat = time.Now()
	f.loadedOnce = true
	f.reloadLocked()
}

func (f *FS) reloadLocked() {
	fi, err := os.Stat(f.cachePath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[webdav] stat library cache %q: %v", f.cachePath, err)
		}
		f.ensureRootLocked()
		return
	}
	if !f.lastMod.IsZero() && fi.ModTime().Equal(f.lastMod) {
		return // unchanged since the last successful load
	}
	cache, err := f.load()
	if err != nil {
		log.Printf("[webdav] reload library cache %q: %v", f.cachePath, err)
		f.ensureRootLocked()
		return
	}
	f.lastMod = fi.ModTime()
	if cache == nil {
		f.root = newDir("")
		return
	}
	f.root = buildTree(cache.Items)
}

func (f *FS) ensureRootLocked() {
	if f.root == nil {
		f.root = newDir("")
	}
}

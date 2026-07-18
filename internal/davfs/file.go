package davfs

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"time"

	"golang.org/x/net/webdav"
)

var (
	errIsDir   = errors.New("davfs: is a directory")
	errNotDir  = errors.New("davfs: not a directory")
	errDirSeek = errors.New("davfs: invalid directory seek")
)

// Both file kinds satisfy webdav.File (http.File + io.Writer). Writes always
// fail — this is the read-only contract at the file layer.
var (
	_ webdav.File = (*dirFile)(nil)
	_ webdav.File = (*realFile)(nil)
)

// nodeInfo is a synthetic fs.FileInfo for virtual directories and cached leaf
// entries (used in directory listings). Directories are r-x, files r--.
type nodeInfo struct {
	name    string
	size    int64
	modTime time.Time
	isDir   bool
}

func (i nodeInfo) Name() string { return i.name }
func (i nodeInfo) Size() int64  { return i.size }
func (i nodeInfo) Mode() fs.FileMode {
	if i.isDir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}
func (i nodeInfo) ModTime() time.Time { return i.modTime }
func (i nodeInfo) IsDir() bool        { return i.isDir }
func (i nodeInfo) Sys() any           { return nil }

func (n *node) info() nodeInfo {
	return nodeInfo{name: n.name, size: n.size, modTime: n.modTime, isDir: n.isDir}
}

// dirFile is a synthetic, read-only directory handle over a snapshot of a
// node's children (nodes are immutable once built, so the snapshot never
// races a rebuild).
type dirFile struct {
	info     nodeInfo
	children []fs.FileInfo
	pos      int
}

func newDirFile(n *node) *dirFile {
	kids := make([]fs.FileInfo, 0, len(n.order))
	for _, name := range n.order {
		kids = append(kids, n.children[name].info())
	}
	return &dirFile{info: n.info(), children: kids}
}

func (d *dirFile) Close() error               { return nil }
func (d *dirFile) Read([]byte) (int, error)   { return 0, errIsDir }
func (d *dirFile) Write([]byte) (int, error)  { return 0, os.ErrPermission }
func (d *dirFile) Stat() (fs.FileInfo, error) { return d.info, nil }

// Seek only supports resetting to the start (whence=io.SeekStart, offset=0),
// which is all http/webdav needs for a directory handle.
func (d *dirFile) Seek(offset int64, whence int) (int64, error) {
	if offset == 0 && whence == io.SeekStart {
		d.pos = 0
		return 0, nil
	}
	return 0, errDirSeek
}

// Readdir mirrors os.File semantics: count<=0 returns all remaining entries;
// count>0 paginates and returns io.EOF once exhausted. webdav's PROPFIND walk
// calls Readdir(0) (all).
func (d *dirFile) Readdir(count int) ([]fs.FileInfo, error) {
	if count <= 0 {
		rest := append([]fs.FileInfo(nil), d.children[d.pos:]...)
		d.pos = len(d.children)
		return rest, nil
	}
	if d.pos >= len(d.children) {
		return nil, io.EOF
	}
	end := d.pos + count
	if end > len(d.children) {
		end = len(d.children)
	}
	out := append([]fs.FileInfo(nil), d.children[d.pos:end]...)
	d.pos = end
	return out, nil
}

// realFile serves a real on-disk media file read-only. Read/Seek/Close are the
// embedded *os.File's (so http.ServeContent's Range handling works verbatim);
// Write is blocked and Stat reports the virtual leaf name.
type realFile struct {
	*os.File
	name string
}

func (r *realFile) Write([]byte) (int, error)          { return 0, os.ErrPermission }
func (r *realFile) Readdir(int) ([]fs.FileInfo, error) { return nil, errNotDir }

func (r *realFile) Stat() (fs.FileInfo, error) {
	fi, err := r.File.Stat()
	if err != nil {
		return nil, err
	}
	return nodeInfo{name: r.name, size: fi.Size(), modTime: fi.ModTime()}, nil
}

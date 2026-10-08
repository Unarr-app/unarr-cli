// Package remotefs exposes remote media as a read-only, snapshot-based filesystem.
// Metadata operations never open the remote source.
package remotefs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/net/webdav"
)

// Entry is immutable once published. Key identifies content independently of a
// temporary delivery URL. Open must produce an independent reader each time.
type Entry struct {
	Path     string                                           `json:"path"`
	Key      string                                           `json:"key"`
	Size     int64                                            `json:"size"`
	Modified time.Time                                        `json:"modified"`
	Open     func(context.Context) (io.ReadSeekCloser, error) `json:"-"`
}

type snapshot struct {
	nodes      map[string]*inode
	generation uint64
}

var snapshotGeneration atomic.Uint64

type inode struct {
	entry    Entry
	dir      bool
	children []fs.FileInfo
	etag     string
}

func (n *inode) Name() string       { return path.Base(n.entry.Path) }
func (n *inode) Size() int64        { return n.entry.Size }
func (n *inode) ModTime() time.Time { return n.entry.Modified }
func (n *inode) IsDir() bool        { return n.dir }
func (n *inode) Sys() any           { return nil }
func (n *inode) Mode() fs.FileMode {
	if n.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}
func (n *inode) ETag(context.Context) (string, error) { return n.etag, nil }

func (n *inode) ContentType(context.Context) (string, error) {
	t := mime.TypeByExtension(path.Ext(n.entry.Path))
	if t == "" {
		t = "application/octet-stream"
	}
	return t, nil
}

// FS swaps complete snapshots atomically. Open directory handles keep their
// original snapshot, including stable ordering, across refreshes.
type FS struct{ current atomic.Pointer[snapshot] }

var _ webdav.FileSystem = (*FS)(nil)

func New() *FS {
	f := &FS{}
	_ = f.Replace(nil)
	return f
}

func validPath(p string) bool {
	return fs.ValidPath(p) && p != "." && !strings.ContainsAny(p, "\\\x00\r\n")
}

// Replace validates the entire new tree before publishing; errors preserve the
// old tree. Duplicate names and file/directory conflicts are never overwritten.
func (f *FS) Replace(entries []Entry) error {
	s := &snapshot{nodes: make(map[string]*inode, 2*len(entries)+1)}
	s.nodes["."] = &inode{entry: Entry{Path: "."}, dir: true}
	for _, e := range entries {
		if err := s.insert(e); err != nil {
			return err
		}
	}
	s.finalize()
	s.generation = snapshotGeneration.Add(1)
	f.current.Store(s)
	return nil
}

func (s *snapshot) insert(e Entry) error {
	if !validPath(e.Path) || e.Size < 0 || e.Key == "" || e.Open == nil {
		return fmt.Errorf("remote filesystem: invalid entry %q", e.Path)
	}
	if _, exists := s.nodes[e.Path]; exists {
		return fmt.Errorf("remote filesystem: duplicate path %q", e.Path)
	}
	s.nodes[e.Path] = &inode{entry: e}
	for p := path.Dir(e.Path); p != "."; p = path.Dir(p) {
		if n, ok := s.nodes[p]; ok {
			if !n.dir {
				return fmt.Errorf("remote filesystem: file is a parent %q", p)
			}
			break
		}
		s.nodes[p] = &inode{entry: Entry{Path: p}, dir: true}
	}
	return nil
}

func (s *snapshot) finalize() {
	for p, n := range s.nodes {
		if p == "." {
			continue
		}
		parent := s.nodes[path.Dir(p)]
		parent.children = append(parent.children, n)
		for d := path.Dir(p); ; d = path.Dir(d) {
			if n.ModTime().After(s.nodes[d].entry.Modified) {
				s.nodes[d].entry.Modified = n.ModTime()
			}
			if d == "." {
				break
			}
		}
	}
	for _, n := range s.nodes {
		sort.Slice(n.children, func(i, j int) bool { return n.children[i].Name() < n.children[j].Name() })
		n.etag = entryETag(n.entry)
	}
}

func entryETag(e Entry) string {
	var storage [256]byte
	b := append(storage[:0], e.Key...)
	b = append(b, ':')
	b = append(b, e.Path...)
	b = append(b, ':')
	b = strconv.AppendInt(b, e.Size, 10)
	b = append(b, ':')
	b = strconv.AppendInt(b, e.Modified.UnixNano(), 10)
	h := sha256.Sum256(b)
	var encoded [34]byte
	encoded[0], encoded[33] = '"', '"'
	hex.Encode(encoded[1:33], h[:16])
	return string(encoded[:])
}

func (f *FS) lookup(name string) (*inode, error) {
	name = strings.Trim(name, "/")
	if name == "" {
		name = "."
	}
	if !fs.ValidPath(name) {
		return nil, os.ErrNotExist
	}
	n := f.current.Load().nodes[name]
	if n == nil {
		return nil, os.ErrNotExist
	}
	return n, nil
}

func (f *FS) Stat(_ context.Context, name string) (fs.FileInfo, error) { return f.lookup(name) }
func (f *FS) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (webdav.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND|os.O_EXCL) != 0 {
		return nil, os.ErrPermission
	}
	n, err := f.lookup(name)
	if err != nil {
		return nil, err
	}
	return &file{ctx: ctx, node: n}, nil
}
func (*FS) Mkdir(context.Context, string, os.FileMode) error { return os.ErrPermission }
func (*FS) RemoveAll(context.Context, string) error          { return os.ErrPermission }
func (*FS) Rename(context.Context, string, string) error     { return os.ErrPermission }

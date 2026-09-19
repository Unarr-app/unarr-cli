package remotefs

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
)

type file struct {
	ctx    context.Context
	node   *inode
	reader io.ReadSeekCloser
	pos    int64
	dirPos int
	closed bool
}

func (f *file) Stat() (fs.FileInfo, error) {
	if f.closed {
		return nil, os.ErrClosed
	}
	return f.node, nil
}
func (f *file) Write([]byte) (int, error) { return 0, os.ErrPermission }
func (f *file) Close() error {
	if f.closed {
		return nil
	}
	f.closed = true
	if f.reader != nil {
		return f.reader.Close()
	}
	return nil
}
func (f *file) Readdir(count int) ([]fs.FileInfo, error) {
	if f.closed {
		return nil, os.ErrClosed
	}
	if !f.node.dir {
		return nil, errors.New("not a directory")
	}
	kids := f.node.children
	if count > 0 && f.dirPos >= len(kids) {
		return nil, io.EOF
	}
	end := len(kids)
	if count > 0 && count < end-f.dirPos {
		end = f.dirPos + count
	}
	// Copy the slice of immutable entries, not the entries. Callers may mutate
	// their slice without corrupting every other open directory handle.
	out := append([]fs.FileInfo(nil), kids[f.dirPos:end]...)
	f.dirPos = end
	return out, nil
}

func seekPosition(pos, size, offset int64, whence int) (int64, error) {
	var base int64
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		base = pos
	case io.SeekEnd:
		base = size
	default:
		return 0, errors.New("invalid seek origin")
	}
	if offset > 0 && base > math.MaxInt64-offset || offset < 0 && offset < -base {
		return 0, errors.New("invalid seek offset")
	}
	return base + offset, nil
}

func (f *file) Seek(offset int64, whence int) (int64, error) {
	if f.closed {
		return 0, os.ErrClosed
	}
	if f.node.dir {
		if offset != 0 || whence != io.SeekStart {
			return 0, errors.New("invalid directory seek")
		}
		f.dirPos = 0
		return 0, nil
	}
	p, err := seekPosition(f.pos, f.node.Size(), offset, whence)
	if err != nil {
		return 0, err
	}
	if f.reader != nil {
		if _, err = f.reader.Seek(p, io.SeekStart); err != nil {
			return 0, err
		}
	}
	f.pos = p
	return p, nil
}

func (f *file) Read(p []byte) (int, error) {
	if f.closed {
		return 0, os.ErrClosed
	}
	if f.node.dir {
		return 0, errors.New("is a directory")
	}
	if err := f.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	remaining := f.node.Size() - f.pos
	if remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	if f.reader == nil {
		r, err := f.node.entry.Open(f.ctx)
		if err != nil {
			return 0, err
		}
		if r == nil {
			return 0, errors.New("remote source returned no reader")
		}
		if _, err = r.Seek(f.pos, io.SeekStart); err != nil {
			_ = r.Close()
			return 0, err
		}
		f.reader = r
	}
	n, err := f.reader.Read(p)
	f.pos += int64(n)
	if err == io.EOF && f.pos < f.node.Size() {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

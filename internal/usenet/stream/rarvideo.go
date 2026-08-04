package stream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
)

// rarVideoReader is an io.ReadSeekCloser over the video file stored inside a
// RarStore. It mirrors the streaming Reader's contract (Seek is network-free,
// SeekEnd knows the exact size up front) so http.ServeContent / ffmpeg drive it
// exactly like a local file. It reads the underlying volume containers through
// per-volume streaming Readers, translating each video offset to a container
// offset via the RarStore extents. It is single-consumer: one goroutine calls
// Read/Seek/Close, as http.ServeContent does.
type rarVideoReader struct {
	ctx context.Context
	rs  *RarStore

	pos int64 // logical read position within the video file

	curVol    int           // volume index the current reader serves, -1 if none
	curReader *readerVolume // open reader for curVol (lazily opened, closed on switch)

	// budget, when set, is the SHARED NNTP byte ceiling handed to every volume
	// reader this stream opens, so a speculative read cannot escape its cap by
	// crossing a volume boundary (each crossing mints a fresh Reader).
	budget *FetchBudget
}

// SetFetchBudget caps the NNTP bytes this stream may pull across ALL its volumes.
// Implements BudgetedReader.
//
// Must be called before the first Read, like Reader.SetFetchBudget: after that,
// prefetch goroutines are reading the budget field and this would be an unguarded
// concurrent write. OpenVideo returns a reader with no volume open yet, so every
// caller is already in that window.
func (v *rarVideoReader) SetFetchBudget(b *FetchBudget) {
	v.budget = b
}

// Read serves bytes at the current position by locating the extent covering it,
// switching to that extent's volume reader, and reading the run's bytes. Reads
// never span an extent boundary in one call — the next Read picks up the next
// extent — which keeps a single Read tied to a single volume.
func (v *rarVideoReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if v.pos >= v.rs.videoSize {
		return 0, io.EOF
	}
	ext, ok := v.findExtent(v.pos)
	if !ok {
		return 0, fmt.Errorf("usenet rar reader: no extent covers offset %d", v.pos)
	}
	if err := v.useVolume(ext.volIndex); err != nil {
		return 0, err
	}
	containerOff := ext.dataOffset + (v.pos - ext.videoStart)
	if _, err := v.curReader.r.Seek(containerOff, io.SeekStart); err != nil {
		return 0, fmt.Errorf("usenet rar reader: seek volume %d: %w", ext.volIndex, err)
	}

	remaining := ext.videoStart + ext.length - v.pos
	limit := int64(len(p))
	if remaining < limit {
		limit = remaining
	}
	n, err := v.curReader.r.Read(p[:limit])
	v.pos += int64(n)
	if errors.Is(err, io.EOF) && n > 0 {
		err = nil // partial read at an article boundary is fine; caller reads again
	}
	return n, err
}

// Seek moves the logical position. SeekEnd uses the exact video size known from
// the probe, so it costs no network and never over/under-reports (unlike the
// encoded-size estimate).
func (v *rarVideoReader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = v.pos + offset
	case io.SeekEnd:
		abs = v.rs.videoSize + offset
	default:
		return 0, fmt.Errorf("usenet rar reader: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, errors.New("usenet rar reader: negative position")
	}
	v.pos = abs
	return abs, nil
}

// Close releases the currently open volume reader. The RarStore itself owns no
// long-lived readers, so nothing else needs closing.
func (v *rarVideoReader) Close() error {
	return v.closeCurrent()
}

// findExtent returns the extent whose video range covers off. Extents are sorted
// by videoStart, so a binary search locates it in O(log n).
func (v *rarVideoReader) findExtent(off int64) (extent, bool) {
	exts := v.rs.extents
	i := sort.Search(len(exts), func(k int) bool { return exts[k].videoStart > off }) - 1
	if i < 0 || i >= len(exts) {
		return extent{}, false
	}
	e := exts[i]
	if off >= e.videoStart && off < e.videoStart+e.length {
		return e, true
	}
	return extent{}, false
}

// useVolume makes volIndex the current reader, opening it (and closing the
// previous one) only when the position crosses into a different volume. Reading a
// file that fits in one volume therefore opens exactly one reader for the whole
// stream.
func (v *rarVideoReader) useVolume(volIndex int) error {
	if v.curVol == volIndex && v.curReader != nil {
		return nil
	}
	if err := v.closeCurrent(); err != nil {
		return err
	}
	// The shared byte ceiling is handed to the fresh volume reader — otherwise a
	// budgeted speculative read would reset its cost at every volume boundary.
	rv, err := newReaderVolume(v.ctx, v.rs.fetcher, v.rs.volumes[volIndex], v.budget)
	if err != nil {
		return fmt.Errorf("usenet rar reader: open volume %d: %w", volIndex, err)
	}
	v.curReader = rv
	v.curVol = volIndex
	return nil
}

// closeCurrent closes and forgets the open volume reader, if any.
func (v *rarVideoReader) closeCurrent() error {
	if v.curReader == nil {
		return nil
	}
	err := v.curReader.close()
	v.curReader = nil
	v.curVol = -1
	return err
}

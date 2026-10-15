package backup

import (
	"fmt"
	"io"
)

// Size bounds, shared by Create and Open: Create refuses — before writing
// anything — an archive Open would refuse, so every backup it writes restores.
//
// maxContentSize bounds the members together, decompressed, manifest included:
// it is the zip-bomb guard and what a restore holds in memory. Only the queue
// grows. The daemon writes it at under 1 KB a task (40,000 debrid tasks are
// 33 MB) and rewrites the whole file on every change, so a working queue stays
// far below 128 MiB: tens of thousands of tasks fit several times over. Memory
// stays proportional — measured on those 40,000 tasks, 245 MB peak for a
// secret-free create (the redaction parses the queue), about 110 MB for an
// encrypted create or any restore.
//
// A var only so tests can probe the edge without writing 128 MiB.
var maxContentSize int64 = 128 << 20

const (
	// archiveFraming is what zip and encryption add on top of the content:
	// member headers, deflate's stored blocks for incompressible data (5 bytes
	// per 64 KiB) and the encryption header and tag — a few KiB at most.
	archiveFraming = 1 << 20
	// maxMembers caps the member count: the manifest and the allowlist, with
	// headroom for a future schema.
	maxMembers = 16
)

// maxArchiveSize bounds the archive file itself, so a stray multi-GB file is
// refused before it is read into memory.
func maxArchiveSize() int64 { return maxContentSize + archiveFraming }

// contentBudget counts member bytes against maxContentSize. Create and Open
// spend one member by member, so they agree on what fits.
type contentBudget struct{ left int64 }

func newContentBudget() *contentBudget { return &contentBudget{left: maxContentSize} }

// spend takes n bytes for member name, or reports that they do not fit.
func (b *contentBudget) spend(name string, n int64) error {
	if n > b.left {
		return errContentTooLarge(name, n)
	}
	b.left -= n
	return nil
}

// errContentTooLarge names the member that broke the bound; size < 0 when it
// is not known (more bytes turned up than were declared).
func errContentTooLarge(name string, size int64) error {
	if size < 0 {
		return fmt.Errorf("%w: %s takes the content past the %s a backup can hold", ErrTooLarge, name, mib(maxContentSize))
	}
	return fmt.Errorf("%w: %s (%s) takes the content past the %s a backup can hold", ErrTooLarge, name, mib(size), mib(maxContentSize))
}

// checkArchiveSize refuses an archive file larger than maxArchiveSize; size < 0
// when only "more than the bound" is known.
func checkArchiveSize(what string, size int64) error {
	switch {
	case size < 0:
		return fmt.Errorf("%w: %s is over the %s an archive can be", ErrTooLarge, what, mib(maxArchiveSize()))
	case size > maxArchiveSize():
		return fmt.Errorf("%w: %s is %s, over the %s an archive can be", ErrTooLarge, what, mib(size), mib(maxArchiveSize()))
	default:
		return nil
	}
}

// readLimited reads r to the end unless it holds more than limit bytes, which
// it reports as over without reading further.
func readLimited(r io.Reader, limit int64) (data []byte, over bool, err error) {
	data, err = io.ReadAll(io.LimitReader(r, limit+1))
	if int64(len(data)) > limit {
		return nil, true, err
	}
	return data, false, err
}

func mib(n int64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }

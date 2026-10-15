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

// The members that do not grow with use get a cap of their own, far below the
// shared bound only the queue needs. config.toml is a settings file: one with
// every table set and commented is under 100 KB, so 4 MiB is forty times any
// real config. The manifest lists two files in under 1 KB. Without these caps a
// crafted archive could hand restore 128 MiB of anything as config.toml — 40
// MiB of NUL bytes deflate to 40 KB — and replace the config with it.
const (
	maxConfigSize   = 4 << 20
	maxManifestSize = 1 << 20
)

// memberCap is the most member name may hold on its own. The queue, and any
// name restore refuses anyway, are only bound by the shared maxContentSize.
func memberCap(name string) int64 {
	switch name {
	case nameConfig:
		return maxConfigSize
	case ManifestName:
		return maxManifestSize
	default:
		return maxContentSize
	}
}

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

// contentBudget counts member bytes against maxContentSize, and each member
// against its own cap. Create and Open spend one member by member, so they
// agree on what fits.
type contentBudget struct{ left int64 }

func newContentBudget() *contentBudget { return &contentBudget{left: maxContentSize} }

// room is the most member name can still take: its own cap or what is left of
// the shared bound, whichever is less.
func (b *contentBudget) room(name string) int64 { return min(memberCap(name), b.left) }

// spend takes n bytes for member name, or reports that they do not fit.
func (b *contentBudget) spend(name string, n int64) error {
	if n > b.room(name) {
		return b.tooLarge(name, n)
	}
	b.left -= n
	return nil
}

// tooLarge names the member that broke a bound, and which one: its own cap
// when that was the tighter, else the shared bound. size < 0 when it is not
// known (more bytes turned up than were declared).
func (b *contentBudget) tooLarge(name string, size int64) error {
	what := name
	if size >= 0 {
		what += " (" + mib(size) + ")"
	}
	if c := memberCap(name); c < maxContentSize && c <= b.left {
		return fmt.Errorf("%w: %s is over the %s limit for that file", ErrTooLarge, what, mib(c))
	}
	return fmt.Errorf("%w: %s takes the content past the %s a backup can hold", ErrTooLarge, what, mib(maxContentSize))
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

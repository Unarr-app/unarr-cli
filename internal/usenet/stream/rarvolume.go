package stream

import (
	"context"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nzb"
)

// volumeSource gives bounded random-access reads over the raw bytes of ONE RAR
// volume plus its exact size. Header parsing only ever touches a few hundred
// header bytes through it, so the whole volume is never downloaded to classify a
// release. It is an interface so parser tests can drive an in-memory byte slice
// with no NNTP at all, while production wraps a streaming Reader.
type volumeSource interface {
	// readAt returns exactly n bytes starting at off, or an error (including a
	// short read past the end of the volume).
	readAt(off, n int64) ([]byte, error)
	// size is the exact byte length of the volume container.
	size() int64
}

// readerVolume adapts a streaming Reader to volumeSource. The Reader's Seek is
// network-free and Read fetches+caches whole articles, so repeated small header
// reads cost at most one article fetch each and typically hit the cache.
type readerVolume struct {
	r  *Reader
	sz int64
}

// newReaderVolume builds a Reader over a RAR volume file for PLAYBACK and
// establishes its exact size (one article fetch via Seek-to-end). Read-ahead
// stays on: this reader streams the video out of the container, so it needs the
// sequential cushion. The caller owns closing it.
// budget is the shared NNTP byte ceiling (nil = unbounded live playback); it is
// applied BEFORE the size probe so even that first article is charged.
func newReaderVolume(ctx context.Context, fetcher ArticleFetcher, f nzb.File, budget *FetchBudget) (*readerVolume, error) {
	return openReaderVolume(ctx, fetcher, f, true, budget)
}

// newProbeVolume builds a Reader over a RAR volume for the HEADER PROBE, with
// read-ahead OFF.
//
// The probe does a handful of small RANDOM reads at the front of the container and
// never streams it, so the sequential-playback cushion fetches ~4 extra articles
// (~3 MB) PER VOLUME that the parser never looks at. On a 99-volume release that
// is ~300 MB of billed Usenet traffic burned just to CLASSIFY the release — before
// anyone has pressed play, and even if the release then turns out not to be
// streamable at all. Playback keeps its cushion via newReaderVolume.
// budget bounds the whole probe (shared across every volume reader it opens), so
// classifying a release cannot walk the set without a ceiling.
func newProbeVolume(ctx context.Context, fetcher ArticleFetcher, f nzb.File, budget *FetchBudget) (*readerVolume, error) {
	return openReaderVolume(ctx, fetcher, f, false, budget)
}

// openReaderVolume is the shared constructor behind newReaderVolume /
// newProbeVolume — one code path, one behavioural difference (the read-ahead
// cushion), so the two call sites can never drift apart.
func openReaderVolume(ctx context.Context, fetcher ArticleFetcher, f nzb.File, readahead bool, budget *FetchBudget) (*readerVolume, error) {
	r := NewReader(ctx, fetcher, f, NewOffsetIndex(f))
	if !readahead {
		r.DisableReadahead()
	}
	r.SetFetchBudget(budget)
	sz, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		_ = r.Close()
		return nil, err
	}
	return &readerVolume{r: r, sz: sz}, nil
}

func (v *readerVolume) size() int64 { return v.sz }

// readAt seeks and reads exactly n bytes. A read that runs off the end of the
// volume surfaces as an error so a truncated/corrupt container is rejected rather
// than silently yielding short data.
func (v *readerVolume) readAt(off, n int64) ([]byte, error) {
	if off < 0 || n < 0 {
		return nil, io.ErrUnexpectedEOF
	}
	// Bound the request against the known volume size BEFORE allocating: n comes
	// from untrusted container bytes (e.g. a RAR5 HeaderSize vint fetched over
	// NNTP), so an oversized/corrupt length must be rejected as a clean read error
	// rather than reaching make([]byte, n) and OOM-crashing the daemon. A valid
	// header always satisfies off+n <= size; computed as v.sz-n to avoid off+n
	// overflow (n may be up to maxInt64).
	if n > v.sz || off > v.sz-n {
		return nil, io.ErrUnexpectedEOF
	}
	if _, err := v.r.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(v.r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func (v *readerVolume) close() error { return v.r.Close() }

// videoExtensions are the container types the streaming path can serve. Anything
// outside this set inside an archive is treated as non-video (a sample, subtitle,
// nfo, …) for the purpose of picking the one file to stream.
var videoExtensions = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".avi": true,
	".mov": true, ".ts": true, ".webm": true, ".wmv": true,
}

// isVideoName reports whether name has a known video container extension.
func isVideoName(name string) bool {
	return videoExtensions[strings.ToLower(filepath.Ext(name))]
}

// rarBaseName strips any directory prefix recorded inside the archive (RAR uses
// backslashes on Windows-created archives), leaving the bare file name used to
// stitch a split file's chunks across volumes.
func rarBaseName(name string) string {
	return filepath.Base(strings.ReplaceAll(name, "\\", "/"))
}

// partVolRe matches new-style RAR5 volume names like "release.part07.rar".
var partVolRe = regexp.MustCompile(`(?i)\.part(\d+)\.rar$`)

// oldVolRe matches classic ".r00"/".s00" continuation volumes.
var oldVolRe = regexp.MustCompile(`(?i)\.([rs])(\d+)$`)

// numVolRe matches split ".001"/".002" volumes.
var numVolRe = regexp.MustCompile(`\.(\d{3,})$`)

// volumeOrder returns a sort key that puts RAR volumes into assembly order for
// both classic (.rar/.r00/.r01) and new-style (.partNN.rar) naming, so an NZB
// listing files out of order still stitches a split file correctly.
func volumeOrder(name string) int {
	if m := partVolRe.FindStringSubmatch(name); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n // part01 -> 1, part02 -> 2, …
	}
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".rar") {
		return 0 // ".rar" is the first classic volume
	}
	if m := oldVolRe.FindStringSubmatch(name); m != nil {
		n, _ := strconv.Atoi(m[2])
		return n + 1 // ".r00" is the second classic volume
	}
	if m := numVolRe.FindStringSubmatch(name); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 1 << 30 // unknown -> sort last, deterministically
}

// sortRarVolumes returns rarFiles ordered by volume index without mutating the
// input slice.
func sortRarVolumes(rarFiles []nzb.File) []nzb.File {
	out := make([]nzb.File, len(rarFiles))
	copy(out, rarFiles)
	sort.SliceStable(out, func(i, j int) bool {
		return volumeOrder(out[i].Filename()) < volumeOrder(out[j].Filename())
	})
	return out
}

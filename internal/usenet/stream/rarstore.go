package stream

import (
	"context"
	"io"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nzb"
)

// probeConcurrencyDefault is the header-probe fan-out used when the fetcher does
// not advertise its connection-pool size. Matches nntp.Client's default pool.
const probeConcurrencyDefault = 10

// concurrencyHinter is optionally implemented by a fetcher backed by a bounded
// connection pool (the NNTP client). probeVolumes fans out no wider than the pool
// — extra goroutines would only block on connection acquisition.
type concurrencyHinter interface{ MaxConcurrency() int }

// probeConcurrency picks the header-probe fan-out for this fetcher, capped to the
// number of volumes.
func probeConcurrency(fetcher ArticleFetcher, volumeCount int) int {
	limit := probeConcurrencyDefault
	if h, ok := fetcher.(concurrencyHinter); ok {
		if m := h.MaxConcurrency(); m > 0 {
			limit = m
		}
	}
	if limit > volumeCount {
		limit = volumeCount
	}
	if limit < 1 {
		limit = 1
	}
	return limit
}

// NotStreamableError marks a release the streaming path must NOT attempt —
// compressed, encrypted, multi-file-ambiguous, or otherwise not a plain STORE
// container. Reason is a human string for the log. Callers use
// errors.Is(err, ErrNotStreamable) to fall back cleanly to the batch download;
// streaming is an opt-in optimisation, never a requirement, so this is a normal
// (logged) outcome, not a failure.
type NotStreamableError struct{ Reason string }

func (e *NotStreamableError) Error() string { return "rar not streamable: " + e.Reason }

// Is lets errors.Is(err, ErrNotStreamable) match any NotStreamableError.
func (e *NotStreamableError) Is(target error) bool { return target == ErrNotStreamable }

// ErrNotStreamable is the sentinel to test against with errors.Is.
var ErrNotStreamable = &NotStreamableError{}

func notStreamable(reason string) error { return &NotStreamableError{Reason: reason} }

// extent maps a byte range of the logical video file onto a byte range inside one
// RAR volume container. Because the archive is STORE, the mapping is a constant
// shift: video byte (videoStart+i) == volume byte (dataOffset+i) for i in
// [0,length).
type extent struct {
	videoStart int64 // start offset within the reconstructed video file
	length     int64 // bytes of video contributed by this volume
	volIndex   int   // which volume (index into RarStore.volumes)
	dataOffset int64 // offset of this run's data within that volume container
}

// RarStore is a classified, streamable RAR release: a plain STORE archive whose
// single video file has been located across its volumes without extracting
// anything. OpenVideo hands back an io.ReadSeekCloser over the internal video
// file, offsets already translated, ready for http.ServeContent / ffmpeg.
type RarStore struct {
	fetcher   ArticleFetcher
	volumes   []nzb.File // rar volumes in assembly order
	extents   []extent   // video runs, sorted by videoStart
	videoName string
	videoSize int64
}

// VideoName is the file name of the video inside the archive.
func (rs *RarStore) VideoName() string { return rs.videoName }

// VideoSize is the exact byte length of the video inside the archive.
func (rs *RarStore) VideoSize() int64 { return rs.videoSize }

// Probe classifies a RAR release by reading only its headers (never the file
// bodies) and returns a RarStore when it is a plain STORE archive with exactly
// one streamable video file. Any other shape — compressed, encrypted, no video,
// or multiple videos — returns a NotStreamableError so the caller downloads it
// via the batch path instead. rarFiles is the NZB's RarFiles(); an empty set is
// itself not streamable.
func Probe(ctx context.Context, fetcher ArticleFetcher, rarFiles []nzb.File) (*RarStore, error) {
	if len(rarFiles) == 0 {
		return nil, notStreamable("no rar volumes")
	}
	volumes := sortRarVolumes(rarFiles)

	chunks, err := probeVolumes(ctx, fetcher, volumes)
	if err != nil {
		return nil, err
	}
	video, err := selectVideo(chunks)
	if err != nil {
		return nil, err
	}
	extents, total, err := buildExtents(video)
	if err != nil {
		return nil, err
	}
	log.Printf("[usenet-stream] rar-store streamable: %s (%d bytes, %d volume(s))",
		video[0].name, total, len(volumes))
	return &RarStore{
		fetcher:   fetcher,
		volumes:   volumes,
		extents:   extents,
		videoName: video[0].name,
		videoSize: total,
	}, nil
}

// probeVolumes parses every volume's headers CONCURRENTLY (bounded by the NNTP
// connection pool) and returns all file chunks in DETERMINISTIC volume order.
//
// Each volume needs one NNTP round-trip (fetch its first article, read the RAR
// headers). Done sequentially that is ~44s for a 99-volume release — the user sits
// on "Iniciando" the whole time (incident 2026-07-19). Fanning the header reads
// across the pool's connections cuts it to a few seconds. The chunk order is
// preserved by writing each volume's chunks into its own index slot and flattening
// in order (never an append race), so buildExtents stitches the video by volume
// index exactly as the sequential version did. First error wins and cancels the
// rest — a not-streamable / faulty volume still fails fast.
func probeVolumes(ctx context.Context, fetcher ArticleFetcher, volumes []nzb.File) ([]rarChunk, error) {
	limit := probeConcurrency(fetcher, len(volumes))

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	perVol := make([][]rarChunk, len(volumes))
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error
	setErr := func(err error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel() // abort other in-flight header reads
		}
		errMu.Unlock()
	}

	start := time.Now()
	log.Printf("[usenet-stream] probing %d volume header(s) (concurrency %d)...", len(volumes), limit)

	for i, f := range volumes {
		// Fail fast: once a volume errored, stop launching. The already-launched
		// goroutines see the cancelled ctx and abort quickly.
		errMu.Lock()
		stop := firstErr != nil
		errMu.Unlock()
		if stop {
			break
		}
		sem <- struct{}{} // blocks until a pool slot is free; every goroutine releases it
		wg.Add(1)
		go func(i int, f nzb.File) {
			defer wg.Done()
			defer func() { <-sem }()
			vs, err := newReaderVolume(ctx, fetcher, f)
			if err != nil {
				setErr(notStreamable("open volume " + f.Filename() + ": " + err.Error()))
				return
			}
			chunks, perr := parseVolume(vs, i)
			_ = vs.close()
			if perr != nil {
				setErr(perr)
				return
			}
			perVol[i] = chunks
		}(i, f)
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}

	var all []rarChunk
	for _, chunks := range perVol {
		all = append(all, chunks...)
	}
	log.Printf("[usenet-stream] probed %d volume header(s) in %s (concurrency %d)",
		len(volumes), time.Since(start).Round(time.Millisecond), limit)
	return all, nil
}

// selectVideo picks the single streamable video file from the parsed chunks. It
// rejects (NotStreamable) an encrypted file, a non-STORE (compressed) video, no
// video at all, or more than one distinct video — all cases where streaming
// would be wrong or ambiguous. The returned chunks are ordered by volume.
func selectVideo(chunks []rarChunk) ([]rarChunk, error) {
	if len(chunks) == 0 {
		return nil, notStreamable("archive contains no files")
	}
	byName := map[string][]rarChunk{}
	var names []string
	for _, c := range chunks {
		if c.encrypted {
			return nil, notStreamable("encrypted file " + c.name)
		}
		if _, seen := byName[c.name]; !seen {
			names = append(names, c.name)
		}
		byName[c.name] = append(byName[c.name], c)
	}

	var videoNames []string
	for _, name := range names {
		if isVideoName(name) {
			videoNames = append(videoNames, name)
		}
	}
	if len(videoNames) == 0 {
		return nil, notStreamable("no video file in archive")
	}
	if len(videoNames) > 1 {
		return nil, notStreamable("multiple video files (ambiguous)")
	}

	group := byName[videoNames[0]]
	for _, c := range group {
		if !c.stored {
			return nil, notStreamable("video is compressed, not stored")
		}
	}
	sort.SliceStable(group, func(i, j int) bool { return group[i].volIndex < group[j].volIndex })
	return group, nil
}

// buildExtents lays the video's per-volume chunks end to end into the logical
// file, returning the extents (sorted by videoStart) and the total size. It
// verifies the stored bytes add up to the header's unpacked size so a
// mis-stitched or truncated set is rejected rather than served wrong.
func buildExtents(group []rarChunk) ([]extent, int64, error) {
	extents := make([]extent, 0, len(group))
	var start int64
	for _, c := range group {
		if c.packSize < 0 {
			return nil, 0, notStreamable("negative pack size")
		}
		extents = append(extents, extent{
			videoStart: start,
			length:     c.packSize,
			volIndex:   c.volIndex,
			dataOffset: c.dataOffset,
		})
		start += c.packSize
	}
	unp := group[0].unpSize
	if unp > 0 && unp != start {
		return nil, 0, notStreamable("stored bytes do not match unpacked size")
	}
	return extents, start, nil
}

// OpenVideo returns a fresh io.ReadSeekCloser over the video file inside the
// archive. Each call opens its own volume readers (single-consumer, like the
// debrid provider), so http.ServeContent can request the stream repeatedly. The
// returned reader translates video offsets to container offsets and reads across
// volume boundaries transparently.
func (rs *RarStore) OpenVideo(ctx context.Context) io.ReadSeekCloser {
	return &rarVideoReader{ctx: ctx, rs: rs, curVol: -1}
}

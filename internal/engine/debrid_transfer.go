package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/library"
)

// partialSuffix marks an in-progress debrid download. The finished file only
// appears under its final name via an atomic rename AFTER every integrity check
// passed, so nothing downstream (organize, the library scan, the reconcile
// sweep, a user browsing the folder) can ever pick up a half-written video
// wearing a finished name. ".part" is one of the extensions the library
// reconcile sweep already recognizes as a partial (library.IsPartialExt), so a
// genuinely orphaned one is reaped by the existing cleanup.
// Derived from the library package so the reconcile sweep (which reaps
// orphaned partials) and this writer can never disagree about what a partial
// download is named.
const partialSuffix = library.PartialSuffix

func partialPath(dest string) string { return dest + partialSuffix }

func partMetaPath(dest string) string { return dest + library.PartMetaSuffix }

// partMeta is the sidecar that records the PROVENANCE of a partial: which URL
// its bytes came from, the server's validators, and the advertised total. A
// resume may only append when the next response provably continues the same
// bytes — same URL, or a validator the server honoured via If-Range. Without
// this check, a retry that re-resolved a different link (provider failover,
// regenerated CDN URL, a re-dispatch of another release with the same
// filename) would splice two different files into one playable-looking but
// corrupt video.
type partMeta struct {
	URL          string `json:"url"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
	TotalBytes   int64  `json:"totalBytes,omitempty"`
	// InfoHash + FileName identify the torrent file behind the link. Debrid
	// serves the torrent's original bytes, so the same (infohash, file) is the
	// same byte stream even when the CDN URL was re-minted — this is what lets
	// a resume survive the server re-resolving a fresh link on re-dispatch
	// (validator-less CDNs would otherwise restart huge downloads from zero).
	InfoHash string `json:"infoHash,omitempty"`
	FileName string `json:"fileName,omitempty"`
}

// ifRangeValidator returns the validator to send in If-Range, preferring a
// strong ETag. Weak ETags (W/…) are not valid in If-Range (RFC 9110 §13.1.5),
// so Last-Modified is the fallback. Empty means the server gave us nothing to
// revalidate with — resuming then requires an identical URL.
func (m *partMeta) ifRangeValidator() string {
	if m == nil {
		return ""
	}
	if m.ETag != "" && !strings.HasPrefix(m.ETag, "W/") {
		return m.ETag
	}
	return m.LastModified
}

func loadPartMeta(path string) (*partMeta, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m partMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m.URL == "" {
		return nil, fmt.Errorf("partial sidecar %s has no URL", path)
	}
	return &m, nil
}

// parseContentRange parses the Content-Range header in both of its shapes:
//
//	bytes <start>-<end>/<total>   satisfied range (total may be "*" → 0)
//	bytes */<total>               unsatisfied range (416); start/end = -1
//
// ok=false means the header was absent or unparseable — the caller must then
// treat the response as unprovable and restart clean. Single parser for the
// header so a tolerance fix can never reach one call site and miss the other.
func parseContentRange(h string) (start, end, total int64, ok bool) {
	if h == "" {
		return 0, 0, 0, false
	}
	if _, err := fmt.Sscanf(h, "bytes %d-%d/%d", &start, &end, &total); err == nil {
		return start, end, total, true
	}
	if _, err := fmt.Sscanf(h, "bytes %d-%d/*", &start, &end); err == nil {
		return start, end, 0, true
	}
	if _, err := fmt.Sscanf(h, "bytes */%d", &total); err == nil {
		return -1, -1, total, true
	}
	return 0, 0, 0, false
}

// debridTransfer carries the per-download state of one debrid HTTPS fetch:
// where it writes, what may be resumed, and what the server promised.
type debridTransfer struct {
	task      *Task
	url       string
	outputDir string // the manager's download dir (disk-space checks + messages)
	dest      string // final path — written ONLY by finalize's rename
	partial   string // dest + ".part", the sole write target during download
	metaPath  string
	meta      *partMeta // validators from a resumable partial; nil = fresh start
	existing  int64     // bytes already on disk in the partial (0 = fresh)
	start     int64     // resume offset the server actually granted
	total     int64     // expected final size (0 = unknown)
	// expected is the byte size the SERVER resolved for this task (the debrid
	// provider's file listing), carried in Task.DirectFileSize. It is the only
	// end-to-end ground truth the transport can't fake: a CDN that advertises a
	// different total is serving a different file, and a CDN that advertises
	// nothing still gets held to this length. 0 = server sent none (older
	// server, provider without sizes) — transport-level checks only.
	expected int64
}

func newDebridTransfer(task *Task, dest, outputDir string) *debridTransfer {
	x := &debridTransfer{
		task:      task,
		url:       task.DirectURL,
		outputDir: outputDir,
		dest:      dest,
		partial:   partialPath(dest),
		metaPath:  partMetaPath(dest),
		expected:  task.DirectFileSize,
	}
	x.loadResumeState()
	// A partial recorded against a different total than the server now expects
	// was downloaded for a DIFFERENT file (a re-resolve that picked another
	// one) — appending would splice. Discard it and start clean.
	if x.expected > 0 && x.meta != nil && x.meta.TotalBytes > 0 && x.meta.TotalBytes != x.expected {
		log.Printf("[%s] partial was for a %s file but the server now expects %s - discarding it",
			task.ShortID(), formatBytes(x.meta.TotalBytes), formatBytes(x.expected))
		x.removeArtifacts()
	}
	return x
}

// loadResumeState decides whether the on-disk partial may be appended to. Only
// a partial WITH a sidecar proving where its bytes came from is resumable; one
// of unknown provenance (no sidecar — an older agent version, a crash between
// writes we know nothing about) is removed so the download starts clean
// instead of splicing. A recorded partial is resumable when the next response
// can be proven to continue the same bytes: same URL, a validator to send in
// If-Range, or the same (infohash, file) behind a re-minted link.
func (x *debridTransfer) loadResumeState() {
	x.meta, x.existing = nil, 0
	fi, err := os.Stat(x.partial)
	if err != nil || fi.Size() == 0 {
		if err != nil {
			// No partial: an orphaned sidecar (crash between the two removes) is
			// stale by definition.
			_ = os.Remove(x.metaPath)
		}
		return
	}
	m, err := loadPartMeta(x.metaPath)
	if err != nil || !x.metaResumable(m) {
		x.removeArtifacts()
		return
	}
	x.meta = m
	x.existing = fi.Size()
}

// metaResumable reports whether the sidecar's provenance lets the partial
// continue under the CURRENT task.
func (x *debridTransfer) metaResumable(m *partMeta) bool {
	if m.URL == x.url || m.ifRangeValidator() != "" {
		return true
	}
	// Re-minted link, no validator: same torrent file = same bytes. Both sides
	// must be known — an empty infohash proves nothing.
	return m.InfoHash != "" && m.InfoHash == x.task.InfoHash &&
		m.FileName != "" && m.FileName == filepath.Base(x.dest)
}

// removePartialArtifacts deletes the in-progress artifacts of dest (the .part
// and its sidecar) — never dest itself. Tolerates absence; logs anything else
// so an undeletable leftover can be reaped later. THE single place that knows
// which files a partial download consists of.
func removePartialArtifacts(shortID, dest string) {
	for _, p := range []string{partialPath(dest), partMetaPath(dest)} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			log.Printf("[%s] could not remove partial artifact %s: %v", shortID, p, err)
		}
	}
}

// removeArtifacts drops this transfer's partial + sidecar and forgets the
// resume state that described them.
func (x *debridTransfer) removeArtifacts() {
	removePartialArtifacts(x.task.ShortID(), x.dest)
	x.meta, x.existing = nil, 0
}

// openStream performs the GET (with Range/If-Range when resuming) and
// normalizes the response. It returns (nil, nil) when a 416 proved the partial
// already holds the complete file — the caller finalizes without downloading.
func (x *debridTransfer) openStream(ctx context.Context) (*http.Response, error) {
	resp, err := x.doRequest(ctx, x.existing > 0)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		// Full body: fresh start, or the server rejected/ignored the resume
		// (If-Range validator changed, no Range support). Either way the partial
		// is superseded — it gets truncated by openPartial.
		x.acceptFull(resp)
		return x.enforceExpectedSize(resp)
	case http.StatusPartialContent:
		if x.acceptPartial(resp) {
			return x.enforceExpectedSize(resp)
		}
		// The 206 does not provably continue our bytes (offset mismatch, total
		// mismatch, unparseable Content-Range). Appending would corrupt — restart.
		log.Printf("[%s] resume response does not match the on-disk partial (Content-Range %q, have %s) - re-downloading clean",
			x.task.ShortID(), resp.Header.Get("Content-Range"), formatBytes(x.existing))
		resp.Body.Close()
		return x.restartFull(ctx)
	case http.StatusRequestedRangeNotSatisfiable:
		return x.handle416(ctx, resp)
	default:
		resp.Body.Close()
		return nil, fmt.Errorf("unexpected HTTP status: %d %s", resp.StatusCode, resp.Status)
	}
}

func (x *debridTransfer) doRequest(ctx context.Context, withRange bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, x.url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if withRange {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", x.existing))
		if v := x.meta.ifRangeValidator(); v != "" {
			// If-Range makes the resume self-validating: the server answers 206
			// only if the entity is unchanged, and falls back to a full 200
			// otherwise — so a regenerated link to DIFFERENT bytes can never be
			// appended to the old partial.
			req.Header.Set("If-Range", v)
		}
	}
	return httpClient.Do(req)
}

// acceptFull records a 200: everything restarts from byte zero.
func (x *debridTransfer) acceptFull(resp *http.Response) {
	x.start, x.existing = 0, 0
	x.total = 0
	if resp.ContentLength > 0 {
		x.total = resp.ContentLength
	}
	x.saveMetaFrom(resp)
}

// acceptPartial validates a 206 against the on-disk partial. It reports false
// when the response cannot be proven to continue the same bytes at the same
// offset — the caller must then restart clean rather than append.
func (x *debridTransfer) acceptPartial(resp *http.Response) bool {
	// start < 0 is the unsatisfied-range ("bytes */N") shape, which a 206 must
	// never carry — treat it as unprovable rather than comparing it to an offset.
	start, _, crTotal, ok := parseContentRange(resp.Header.Get("Content-Range"))
	if !ok || start < 0 || start != x.existing {
		return false
	}
	if x.meta != nil && x.meta.TotalBytes > 0 && crTotal > 0 && crTotal != x.meta.TotalBytes {
		// Same URL but a different advertised size = a different file behind it.
		return false
	}
	x.start = x.existing
	switch {
	case crTotal > 0:
		x.total = crTotal
	case resp.ContentLength > 0:
		x.total = x.existing + resp.ContentLength
	}
	x.saveMetaFrom(resp)
	return true
}

// enforceExpectedSize reconciles the CDN's advertised total with the size the
// SERVER resolved for this task. A link that advertises a DIFFERENT total is
// serving a different file than the one the task was created for (wrong pick
// behind a regenerated link) — refuse up front rather than download gigabytes
// of the wrong bytes and fail at the end. A CDN that advertises nothing gets
// held to the expected length instead, which arms the truncation/overlong
// guards (and progress %) that x.total==0 would otherwise disable.
func (x *debridTransfer) enforceExpectedSize(resp *http.Response) (*http.Response, error) {
	if x.expected <= 0 {
		return resp, nil
	}
	if x.total > 0 && x.total != x.expected {
		resp.Body.Close()
		x.removeArtifacts() // whatever was on disk belongs to that other file
		return nil, integrityErr("size_conflict", "link serves %s but the resolved file is %s — wrong file behind the link",
			formatBytes(x.total), formatBytes(x.expected))
	}
	x.total = x.expected
	return resp, nil
}

// restartFull re-issues the request without Range after a resume could not be
// validated. The stale partial gets truncated by openPartial (start == 0).
func (x *debridTransfer) restartFull(ctx context.Context) (*http.Response, error) {
	x.meta, x.existing = nil, 0
	resp, err := x.doRequest(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("retry http request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("retry unexpected HTTP status: %d %s", resp.StatusCode, resp.Status)
	}
	x.acceptFull(resp)
	return x.enforceExpectedSize(resp)
}

// handle416 deals with "Range Not Satisfiable": either the partial is already
// the complete file (finalize, signalled by (nil, nil)) or its size disagrees
// with the server's and the download restarts clean.
func (x *debridTransfer) handle416(ctx context.Context, resp *http.Response) (*http.Response, error) {
	defer resp.Body.Close()
	if x.existing == 0 {
		return nil, fmt.Errorf("server returned 416 Range Not Satisfiable")
	}
	// The partial can only be adopted as "already complete" if it matches the
	// size the server resolved — a full file of the WRONG length is the wrong file.
	if x.expected > 0 && x.existing != x.expected {
		log.Printf("[%s] partial is %s but the resolved file is %s - re-downloading",
			x.task.ShortID(), formatBytes(x.existing), formatBytes(x.expected))
		return x.restartFull(ctx)
	}
	// Content-Range: bytes */12345 — the server's actual size (unsatisfied-range
	// form, parsed by the same parser as the satisfied one).
	if _, _, serverSize, ok := parseContentRange(resp.Header.Get("Content-Range")); ok && serverSize > 0 && x.existing != serverSize {
		log.Printf("[%s] local size %s != server size %s, re-downloading",
			x.task.ShortID(), formatBytes(x.existing), formatBytes(serverSize))
		return x.restartFull(ctx)
	}
	log.Printf("[%s] file already complete: %s (%s)", x.task.ShortID(), filepath.Base(x.dest), formatBytes(x.existing))
	return nil, nil
}

// saveMetaFrom stages the current response's provenance. It is NOT written to
// disk here: on a full-body restart the old partial still holds the previous
// entity's bytes until openPartial truncates it, and persisting the NEW
// provenance next to the OLD bytes would arm a splicing resume if the process
// died in that window. persistMeta writes it once the partial is opened (and
// truncated where applicable).
func (x *debridTransfer) saveMetaFrom(resp *http.Response) {
	x.meta = &partMeta{
		URL:          x.url,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		TotalBytes:   x.total,
		InfoHash:     x.task.InfoHash,
		FileName:     filepath.Base(x.dest),
	}
}

// persistMeta writes the staged sidecar atomically (temp + rename — a
// truncated sidecar would make loadPartMeta fail and throw away a resumable
// partial). Best-effort: without it the next attempt merely re-downloads from
// scratch instead of resuming; a working download is never failed over it.
func (x *debridTransfer) persistMeta() {
	if x.meta == nil {
		return
	}
	b, err := json.Marshal(x.meta)
	if err == nil {
		tmp := x.metaPath + ".tmp"
		if err = os.WriteFile(tmp, b, 0o644); err == nil {
			err = os.Rename(tmp, x.metaPath)
		}
	}
	if err != nil {
		log.Printf("[%s] could not write partial sidecar %s: %v (resume will restart from scratch)",
			x.task.ShortID(), x.metaPath, err)
	}
}

// finalize publishes the finished partial under its final name. The rename is
// what makes "the file exists at destPath" mean "the download completed and
// passed every check": nothing else ever writes destPath, so a crash leaves at
// most a .part (resumable or reapable) — never a truncated file wearing the
// finished name.
func (x *debridTransfer) finalize(fileName string) (*Result, error) {
	fi, err := os.Stat(x.partial)
	if err != nil {
		return nil, storageErr("stat_failed", filepath.Dir(x.dest),
			"could not read back the finished download %s — is your drive/NAS still connected? (%v)", x.partial, err)
	}
	if err := os.Rename(x.partial, x.dest); err != nil {
		return nil, storageErr("rename_failed", filepath.Dir(x.dest),
			"could not move the finished download into place: %v", err)
	}
	if err := os.Remove(x.metaPath); err != nil && !os.IsNotExist(err) {
		log.Printf("[%s] could not remove partial sidecar %s: %v", x.task.ShortID(), x.metaPath, err)
	}
	return &Result{
		FilePath: x.dest,
		FileName: fileName,
		Method:   MethodDebrid,
		Size:     fi.Size(),
		// Carry the server-resolved size so verify() — the cross-backend
		// backstop — enforces it too, not just this transport.
		ExpectedBytes: x.expected,
	}, nil
}

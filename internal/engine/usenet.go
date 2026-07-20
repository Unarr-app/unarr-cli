package engine

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/usenet/download"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nntp"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nzb"
	"github.com/Unarr-app/unarr-cli/internal/usenet/postprocess"
)

// activeDownload holds the state for a single in-progress usenet download.
type activeDownload struct {
	cancel  context.CancelFunc
	taskDir string                    // populated after MkdirAll; empty before
	tracker *download.ProgressTracker // populated after tracker creation; nil before
}

// UsenetDownloader downloads via Usenet/NZB protocol.
// It searches for NZBs, downloads articles via NNTP, and assembles the final files.
type UsenetDownloader struct {
	apiClient *agent.Client
	enabled   bool // set during initialization based on features

	mu         sync.Mutex
	nntpClient *nntp.Client
	active     map[string]*activeDownload

	// Cached credentials
	credentials *agent.UsenetCredentials
	credExpiry  time.Time

	// Cached NZB search results (from Available → Download)
	nzbCache   map[string]*agent.NzbSearchResult // taskID → best result
	nzbCacheMu sync.RWMutex

	minFreeBytes int64 // disk reserve for the pre-flight space check (0 = reserve disabled)
}

// SetMinFreeBytes sets the free-space reserve enforced before a download starts.
// Call once at construction; 0 disables the reserve (the size-vs-free check still
// runs). See CheckDiskSpace.
func (u *UsenetDownloader) SetMinFreeBytes(n int64) { u.minFreeBytes = n }

// NewUsenetDownloader creates a usenet downloader.
// apiClient is used to call the web API for NZB search, download, and credentials.
func NewUsenetDownloader(apiClient *agent.Client) *UsenetDownloader {
	return &UsenetDownloader{
		apiClient: apiClient,
		enabled:   true,
		active:    make(map[string]*activeDownload),
		nzbCache:  make(map[string]*agent.NzbSearchResult),
	}
}

func (u *UsenetDownloader) Method() DownloadMethod { return MethodUsenet }

// SetEnabled controls whether usenet downloads are available.
func (u *UsenetDownloader) SetEnabled(enabled bool) {
	u.mu.Lock()
	u.enabled = enabled
	u.mu.Unlock()
}

// Available checks if a usenet download is possible for this task.
// Searches NZB indexers by IMDb ID or title and caches the result.
func (u *UsenetDownloader) Available(ctx context.Context, task *Task) (bool, error) {
	u.mu.Lock()
	enabled := u.enabled
	u.mu.Unlock()

	if !enabled {
		return false, nil
	}

	// Need at least an IMDb ID or title to search
	if task.IMDbID == "" && task.Title == "" {
		return false, nil
	}

	// If task has pre-resolved NZB ID, it's available
	if task.NzbID != "" {
		return true, nil
	}

	// Search NZB indexers
	result, err := u.searchBestNzb(ctx, task)
	if err != nil {
		return false, nil // search failure = not available (don't error out)
	}
	if result == nil {
		return false, nil
	}

	// Cache for Download()
	u.nzbCacheMu.Lock()
	u.nzbCache[task.ID] = result
	u.nzbCacheMu.Unlock()

	return true, nil
}

// Download performs the full usenet download pipeline:
// search NZB → download NZB file → parse → NNTP download → assemble → post-process.
func (u *UsenetDownloader) Download(ctx context.Context, task *Task, outputDir string, progressCh chan<- Progress) (*Result, error) {
	// Create cancellable context
	dlCtx, cancel := context.WithCancel(ctx)

	dl := &activeDownload{cancel: cancel}
	u.mu.Lock()
	u.active[task.ID] = dl
	u.mu.Unlock()

	defer func() {
		u.mu.Lock()
		delete(u.active, task.ID)
		u.mu.Unlock()
		cancel()
	}()

	shortID := task.ID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}

	// Step 1: Get NZB ID (from cache, task, or search)
	nzbID, nzbTitle, err := u.resolveNzbID(dlCtx, task)
	if err != nil {
		return nil, fmt.Errorf("resolve NZB: %w", err)
	}

	log.Printf("[%s] NZB: %s", shortID, nzbTitle)

	// Step 2: Download NZB file (or use cached version for resume)
	resumeDir := filepath.Join(config.DataDir(), "resume")
	nzbCachePath := filepath.Join(resumeDir, task.ID+".nzb")

	nzbData, err := os.ReadFile(nzbCachePath)
	if err != nil {
		// Not cached — download from server
		nzbData, err = u.apiClient.DownloadNzb(dlCtx, nzbID)
		if err != nil {
			return nil, fmt.Errorf("download NZB: %w", err)
		}
		// Cache for future resume (best-effort — download still works without cache)
		if mkErr := os.MkdirAll(resumeDir, 0o755); mkErr != nil {
			log.Printf("[%s] resume dir create failed: %v", shortID, mkErr)
		} else if wErr := os.WriteFile(nzbCachePath, nzbData, 0o644); wErr != nil {
			log.Printf("[%s] NZB cache write failed: %v", shortID, wErr)
		}
	} else {
		log.Printf("[%s] using cached NZB", shortID)
	}

	// Step 3: Parse NZB
	nzbFile, err := nzb.ParseBytes(nzbData)
	if err != nil {
		return nil, fmt.Errorf("parse NZB: %w", err)
	}

	totalBytes := nzbFile.TotalBytes()
	totalSegs := nzbFile.TotalSegments()
	log.Printf("[%s] NZB parsed: %d files, %d segments, %s",
		shortID, len(nzbFile.Files), totalSegs, formatBytes(totalBytes))

	// Step 3.5: Resume support — load or create progress tracker
	tracker := download.NewProgressTracker(task.ID, nzbFile, resumeDir)
	resumed, _ := tracker.Load()
	if resumed {
		log.Printf("[%s] resuming usenet download (%d/%d segments completed)",
			shortID, tracker.TotalCompleted(), totalSegs)

		// Publish the resumed position IMMEDIATELY. Everything between here and
		// the downloader's first 500 ms progress tick — credential fetch, NNTP
		// connect+TLS+AUTH, mkdir — can take seconds, and until then the web
		// only has the zeroes that the resolving/downloading transitions
		// reported. A user who hits Retry on a download that is 80 % on disk
		// must not watch it claim 0 %.
		var resumedBytes int64
		for i, f := range nzbFile.Files {
			resumedBytes += tracker.CompletedBytes(i, f.Segments)
		}
		p := Progress{DownloadedBytes: resumedBytes, TotalBytes: totalBytes}
		task.UpdateProgress(p)
		select {
		case progressCh <- p:
		default:
		}
	} else {
		// Pre-flight disk-space guard on a fresh download (a resume already has
		// its partial bytes on disk; ENOSPC stays the backstop there).
		if err := CheckDiskSpace(outputDir, totalBytes, u.minFreeBytes); err != nil {
			return nil, err
		}
	}

	// Always flush progress on exit — covers graceful shutdown, SIGTERM,
	// error returns, and shutdown-timeout scenarios. The atomic write
	// (tmp+rename) ensures the file is never corrupted even on hard kill.
	defer tracker.Flush()

	// Step 4: Get NNTP credentials and connect
	creds, err := u.getCredentials(dlCtx)
	if err != nil {
		return nil, fmt.Errorf("get credentials: %w", err)
	}

	nntpClient, err := u.getOrCreateNNTP(dlCtx, creds)
	if err != nil {
		return nil, fmt.Errorf("NNTP connect: %w", err)
	}

	log.Printf("[%s] NNTP: %s", shortID, nntpClient.Status())

	// Step 5: Create download directory for this task
	taskDir := filepath.Join(outputDir, sanitizeDir(task.Title))
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		return nil, fmt.Errorf("create dir: %w", err)
	}

	// Register tracker and taskDir for Cancel() cleanup
	u.mu.Lock()
	dl.taskDir = taskDir
	dl.tracker = tracker
	u.mu.Unlock()

	// Step 6: Download all files via NNTP
	segDl := download.NewDownloader(nntpClient)

	// Articles age off retention unevenly, so a mature release commonly has a
	// few dead segments. When the poster shipped parity, those holes are
	// exactly what par2 reconstructs — aborting the entire download on the
	// first 430 throws away a recoverable release and wastes everything already
	// fetched. Without parity there is nothing to repair with, so the default
	// zero tolerance (fail fast) stands.
	if nzbFile.HasPar2() {
		segDl.MissingTolerance = missingSegmentTolerance
	}

	// Bridge download.Progress to engine.Progress
	dlProgressCh := make(chan download.Progress, 16)
	go func() {
		for dp := range dlProgressCh {
			p := Progress{
				DownloadedBytes: dp.BytesDownloaded,
				TotalBytes:      dp.BytesTotal,
				SpeedBps:        dp.SpeedBps,
				FileName:        dp.FileName,
			}
			if dp.BytesTotal > 0 {
				p.ETA = int(float64(dp.BytesTotal-dp.BytesDownloaded) / float64(max(dp.SpeedBps, 1)))
			}
			task.UpdateProgress(p)
			select {
			case progressCh <- p:
			default:
			}
		}
	}()

	downloadedFiles, missingSegs, err := segDl.DownloadNZB(dlCtx, nzbFile, taskDir, tracker, dlProgressCh)
	if len(missingSegs) > 0 {
		// Name a few: "which articles died" is the first thing needed to tell a
		// provider-retention problem from a bad post, and it never reaches the
		// server (only the count does).
		log.Printf("[%s] %d/%d segments unavailable on the server — relying on par2 recovery (first: %s)",
			shortID, len(missingSegs), totalSegs, describeMissing(missingSegs, 3))
	}

	// Step 6.5: parity index — fetch the main .par2 up front (small: file
	// checksums, no recovery data) so post-processing can actually verify the
	// delivery. Recovery volumes stay lazy behind Options.FetchParity and are
	// only fetched when verification detects damage.
	if err == nil {
		downloadPar2Index(dlCtx, segDl, nzbFile, taskDir, downloadedFiles)
	}
	close(dlProgressCh)

	if err != nil {
		return nil, fmt.Errorf("NNTP download: %w", err)
	}

	// Step 7: Post-processing (par2, extract, cleanup)
	log.Printf("[%s] post-processing...", shortID)

	// Use password from NZB meta (embedded in file), or from task (user-provided)
	password := nzbFile.Password
	if task.NzbPassword != "" {
		password = task.NzbPassword // user-provided overrides NZB meta
	}
	if password != "" {
		log.Printf("[%s] NZB has password: %s", shortID, password)
	}
	ppResult, err := postprocess.Process(taskDir, downloadedFiles, postprocess.Options{
		Password: password,
		Cleanup:  true,
		// Lazy parity: the recovery volumes are only downloaded when the index
		// verify detects damage — the happy path costs one small .par2.
		FetchParity: func() (map[string]string, error) {
			vols := nzbFile.Par2VolumeFiles()
			if len(vols) == 0 {
				return nil, fmt.Errorf("NZB ships no par2 recovery volumes")
			}
			return segDl.DownloadPar2Files(dlCtx, vols, taskDir, nil)
		},
	})
	if err != nil {
		// Password error is special — report clearly
		if _, ok := err.(*postprocess.PasswordError); ok {
			return nil, fmt.Errorf("archive is password protected (set password in download options)")
		}
		return nil, fmt.Errorf("post-process: %w", err)
	}

	if ppResult.Repaired {
		log.Printf("[%s] par2: repair was needed and successful", shortID)
	}
	if ppResult.Extracted {
		log.Printf("[%s] extracted archive", shortID)
	}
	if ppResult.VerifyNote != "" {
		// Degraded verification (par2 missing / transient probe error): surface it
		// loudly so the delivered file isn't silently assumed good.
		log.Printf("[%s] WARNING: %s", shortID, ppResult.VerifyNote)
	}
	// We KNOWINGLY left holes in the payload, so anything short of par2 actively
	// vouching for the result is a corrupt delivery.
	//
	// The test is Verified, NOT an empty VerifyNote: an empty note also means
	// "no parity was checked at all", which is exactly what happens when the
	// par2 index fails to download (downloadPar2Index degrades to a warning) —
	// runPar2Step never runs, nothing is flagged, and a zero-filled file would
	// sail through as complete.
	if len(missingSegs) > 0 && !ppResult.Verified {
		ppResult.Corrupt = true
		ppResult.VerifyNote = fmt.Sprintf(
			"%d segments were unavailable and par2 did not confirm a repair (%s)",
			len(missingSegs), verifyStateNote(ppResult))
	}
	if ppResult.Corrupt {
		// Invalidate the resume state of the files par2 named as broken — and
		// ONLY those. Without this the manager's "re-download clean" retry was a
		// no-op: every segment was still marked done, so the next attempt
		// fetched nothing and failed par2 identically, three times over. With
		// it, the retry re-fetches the damaged files while a multi-file release
		// keeps every volume parity confirmed intact.
		invalidateDamaged(tracker, nzbFile, ppResult.DamagedFiles, shortID)

		// par2 DEFINITIVELY confirmed unrepairable damage — fail as an integrity
		// error so the manager re-downloads clean instead of completing a corrupt
		// release (symmetric with the debrid/torrent guards).
		return nil, integrityErr("par2_failed", "usenet delivery is corrupt: %s", ppResult.VerifyNote)
	}

	finalPath := ppResult.FinalPath
	if finalPath == "" {
		// Fallback: use the task directory
		finalPath = taskDir
	}

	// Step 7.5: de-obfuscation fallback. par2 repair already renames misnamed
	// files when parity ships and repair ran; when it didn't (no parity, verify
	// OK on an intact-but-hex-named post, extraction produced a hex name), fall
	// back to the release title so organize/library get a meaningful filename.
	finalPath = maybeDeobfuscate(finalPath, nzbTitle, task.Title)

	// Force the delivered file(s) to durable storage before reporting success.
	// Symmetric with the debrid path (2026-06-15 NFS incident): the prod download
	// dir is a network mount, and post-processing reads the data back for par2 from
	// the page cache while the write-back to the server can still lag — a later open
	// (organize, stream, ffprobe) would then see a short file. fsync commits it now
	// and surfaces a write-back error here, where it's actionable.
	if err := syncTree(finalPath); err != nil {
		return nil, fmt.Errorf("flush to disk failed (write-back/network-mount error): %w", err)
	}

	// Get final file size — after the durable flush, so the size is real. Walk
	// directories (multi-file releases) instead of reporting the dir inode size.
	var finalSize int64
	if fi, err := os.Stat(finalPath); err == nil {
		if fi.IsDir() {
			finalSize, _ = dirSize(finalPath)
		} else {
			finalSize = fi.Size()
		}
	}
	if finalSize == 0 {
		return nil, fmt.Errorf("usenet delivery is empty after post-processing: %s", finalPath)
	}

	// Clean up resume state on successful completion
	tracker.Remove()

	return &Result{
		FilePath: finalPath,
		FileName: filepath.Base(finalPath),
		Method:   MethodUsenet,
		Size:     finalSize,
	}, nil
}

// Pause cancels an in-progress download but keeps files.
func (u *UsenetDownloader) Pause(taskID string) error {
	u.mu.Lock()
	dl := u.active[taskID]
	u.mu.Unlock()
	if dl != nil {
		dl.cancel()
	}
	return nil
}

// Cancel aborts an in-progress download and removes partial files + resume state.
func (u *UsenetDownloader) Cancel(taskID string) error {
	// Read all fields under the lock — Download() writes tracker and taskDir under
	// the same lock, so we must hold it while reading to avoid a data race.
	u.mu.Lock()
	dl := u.active[taskID]
	var tracker *download.ProgressTracker
	var taskDir string
	if dl != nil {
		tracker = dl.tracker
		taskDir = dl.taskDir
	}
	u.mu.Unlock()

	if dl == nil {
		return nil
	}

	// Cancel context first — workers will stop and release file handles
	dl.cancel()

	// Remove resume state (best-effort)
	if tracker != nil {
		tracker.Remove()
	}

	// Remove partial download directory in background (can be slow for large dirs)
	if taskDir != "" {
		go os.RemoveAll(taskDir)
	}

	return nil
}

// Shutdown closes the NNTP connection pool.
func (u *UsenetDownloader) Shutdown(_ context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	// Cancel all active downloads
	for id, dl := range u.active {
		dl.cancel()
		delete(u.active, id)
	}

	// Close NNTP
	if u.nntpClient != nil {
		u.nntpClient.Close()
		u.nntpClient = nil
	}

	return nil
}

// --- Internal helpers ---

func (u *UsenetDownloader) searchBestNzb(ctx context.Context, task *Task) (*agent.NzbSearchResult, error) {
	params := agent.NzbSearchParams{
		Limit: 10,
	}

	if task.IMDbID != "" {
		params.IMDbID = task.IMDbID
	} else {
		params.Query = task.Title
	}

	resp, err := u.apiClient.SearchNzbs(ctx, params)
	if err != nil {
		return nil, err
	}

	if len(resp.Results) == 0 {
		return nil, nil
	}

	// Pick best match: prefer largest size (likely best quality), then most grabs
	best := &resp.Results[0]
	for i := 1; i < len(resp.Results); i++ {
		r := &resp.Results[i]
		if r.Size > best.Size {
			best = r
		} else if r.Size == best.Size && r.Grabs > best.Grabs {
			best = r
		}
	}

	return best, nil
}

func (u *UsenetDownloader) resolveNzbID(ctx context.Context, task *Task) (string, string, error) {
	// Priority 1: Task has pre-resolved NZB ID
	if task.NzbID != "" {
		return task.NzbID, task.Title, nil
	}

	// Priority 2: Check cache from Available()
	u.nzbCacheMu.RLock()
	cached, ok := u.nzbCache[task.ID]
	u.nzbCacheMu.RUnlock()
	if ok {
		// Clean cache entry
		u.nzbCacheMu.Lock()
		delete(u.nzbCache, task.ID)
		u.nzbCacheMu.Unlock()
		return cached.NzbID, cached.Title, nil
	}

	// Priority 3: Search now
	result, err := u.searchBestNzb(ctx, task)
	if err != nil {
		return "", "", err
	}
	if result == nil {
		return "", "", fmt.Errorf("no NZB found for %q (IMDb: %s)", task.Title, task.IMDbID)
	}
	return result.NzbID, result.Title, nil
}

func (u *UsenetDownloader) getCredentials(ctx context.Context) (*agent.UsenetCredentials, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	// Use cached credentials if still valid
	if u.credentials != nil && time.Now().Before(u.credExpiry) {
		return u.credentials, nil
	}

	creds, err := u.apiClient.GetUsenetCredentials(ctx)
	if err != nil {
		return nil, err
	}

	u.credentials = creds
	u.credExpiry = time.Now().Add(5 * time.Minute)
	return creds, nil
}

func (u *UsenetDownloader) getOrCreateNNTP(ctx context.Context, creds *agent.UsenetCredentials) (*nntp.Client, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.nntpClient != nil {
		return u.nntpClient, nil
	}

	maxConns := creds.MaxConnections
	if maxConns <= 0 {
		maxConns = 10
	}

	client := nntp.NewClient(nntp.Config{
		Host:           creds.Host,
		Port:           creds.Port,
		SSL:            creds.SSL,
		TLSServerName:  creds.TLSServerName,
		Username:       creds.Username,
		Password:       creds.Password,
		MaxConnections: maxConns,
	})

	if err := client.Connect(ctx); err != nil {
		return nil, err
	}

	u.nntpClient = client
	return client, nil
}

// missingSegmentTolerance is the fraction of a file's segments allowed to be
// unavailable before the download is abandoned, applied only when the NZB ships
// par2. Posters typically include 5-10% recovery, so 5% stays inside what
// parity can actually reconstruct: past that, finishing the download only to
// fail verification wastes the user's bandwidth.
const missingSegmentTolerance = 0.05

// describeMissing renders up to max unavailable segments as "file#number
// (message-id)", for the operator log.
func describeMissing(missing []download.MissingSegment, max int) string {
	if len(missing) > max {
		missing = missing[:max]
	}
	parts := make([]string, 0, len(missing))
	for _, m := range missing {
		parts = append(parts, fmt.Sprintf("%s#%d (%s: %v)", m.File, m.Number, m.MessageID, m.Err))
	}
	return strings.Join(parts, "; ")
}

// verifyStateNote describes why a delivery is unverified, for the error message.
func verifyStateNote(r *postprocess.Result) string {
	if r.VerifyNote != "" {
		return r.VerifyNote
	}
	return "no par2 verification ran — parity was unavailable"
}

// resetAllContent invalidates the resume state of every non-parity file, for a
// genuinely clean retry. Parity is excluded: those files are re-fetched on
// demand and self-verifying.
func resetAllContent(tracker *download.ProgressTracker, nzbFile *nzb.NZB) {
	for i, f := range nzbFile.Files {
		if strings.EqualFold(filepath.Ext(f.Filename()), ".par2") {
			continue
		}
		tracker.ResetFile(i)
	}
}

// invalidateDamaged clears the resume bits of the NZB files par2 reported as
// damaged, so the next attempt re-fetches exactly those and keeps the rest.
//
// It falls back to invalidating EVERY content file both when par2 named no file
// and when none of the names it gave matched the NZB. That second case is not
// hypothetical: obfuscated posts (the ones maybeDeobfuscate exists for) carry
// hex filenames in the NZB while par2 reports the original names, so the match
// finds nothing. Without the fallback the retry resets nothing, re-fetches
// nothing and fails par2 identically three times — the exact no-op bug this
// function was added to kill. Re-downloading too much still converges;
// re-downloading nothing never does.
func invalidateDamaged(tracker *download.ProgressTracker, nzbFile *nzb.NZB, damaged []string, shortID string) {
	if tracker == nil {
		return
	}

	hit := 0
	if len(damaged) > 0 {
		want := make(map[string]bool, len(damaged))
		for _, name := range damaged {
			want[strings.ToLower(filepath.Base(name))] = true
		}
		for i, f := range nzbFile.Files {
			if want[strings.ToLower(filepath.Base(f.Filename()))] {
				tracker.ResetFile(i)
				hit++
			}
		}
	}

	switch {
	case hit > 0:
		log.Printf("[%s] par2 reported %d damaged file(s); invalidated %d for re-download: %s",
			shortID, len(damaged), hit, strings.Join(damaged, ", "))
	case len(damaged) == 0:
		log.Printf("[%s] par2 named no damaged file — invalidating all content files for a clean retry", shortID)
		resetAllContent(tracker, nzbFile)
	default:
		log.Printf("[%s] par2 named %d damaged file(s) that match no NZB entry (obfuscated post?) — invalidating all content files: %s",
			shortID, len(damaged), strings.Join(damaged, ", "))
		resetAllContent(tracker, nzbFile)
	}

	// The invalidation is only real once it reaches disk: if this write is lost,
	// the retry reloads the old all-done bitset and is a no-op again.
	if err := tracker.Flush(); err != nil {
		log.Printf("[%s] WARNING: could not persist the par2 invalidation — the retry may resume as if intact: %v", shortID, err)
	}
}

// downloadPar2Index fetches the main .par2 (small: checksums only, no recovery
// data) next to the content so post-processing can verify the delivery. A
// failure degrades to an unverified delivery (logged loudly), never an error —
// parity is optional. No-op when the NZB ships no parity.
func downloadPar2Index(ctx context.Context, segDl *download.Downloader, nzbFile *nzb.NZB, taskDir string, downloadedFiles map[string]string) {
	idx := nzbFile.Par2IndexFile()
	if idx == nil {
		return
	}
	par2Files, err := segDl.DownloadPar2Files(ctx, []nzb.File{*idx}, taskDir, nil)
	if err != nil {
		log.Printf("[usenet] WARNING: par2 index download failed — delivery will be UNVERIFIED: %v", err)
		return
	}
	for name, path := range par2Files {
		downloadedFiles[name] = path
	}
}

// maybeDeobfuscate renames an obfuscated main file (long hex-like base —
// common on usenet posts dodging takedowns) to the release title, keeping the
// extension. Returns the (possibly new) path; on any conflict or error the
// original path is kept — a working obfuscated file beats a failed rename.
func maybeDeobfuscate(path, releaseTitle, taskTitle string) string {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return path
	}
	base := filepath.Base(path)
	if !nzb.IsObfuscatedName(base) {
		return path
	}
	title := releaseTitle
	if title == "" {
		title = taskTitle
	}
	ext := filepath.Ext(base)
	// Avoid "Title.mkv.mkv" when the release title already carries the extension.
	if strings.EqualFold(filepath.Ext(title), ext) {
		title = strings.TrimSuffix(title, filepath.Ext(title))
	}
	title = sanitizeDir(title)
	if title == "" || title == "usenet_download" {
		return path
	}
	newPath := filepath.Join(filepath.Dir(path), title+ext)
	if newPath == path {
		return path
	}
	if _, err := os.Stat(newPath); err == nil {
		// Target exists — don't clobber.
		return path
	}
	if err := os.Rename(path, newPath); err != nil {
		log.Printf("[usenet] de-obfuscation rename failed (keeping %s): %v", base, err)
		return path
	}
	log.Printf("[usenet] de-obfuscated %s -> %s", base, filepath.Base(newPath))
	return newPath
}

func sanitizeDir(name string) string {
	if name == "" {
		return "usenet_download"
	}
	for _, c := range []string{"/", "\\", ":", "*", "?", "\"", "<", ">", "|"} {
		name = strings.ReplaceAll(name, c, "_")
	}
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}

// syncTree fsyncs path so its data is durable before the download is treated as
// complete. For a directory (multi-file release) it fsyncs every regular file
// underneath. A Sync error is returned, not swallowed — on a network mount a
// failed write-back must fail the download instead of leaving a truncated file.
func syncTree(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return syncFile(path)
	}
	return filepath.Walk(path, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		return syncFile(p)
	})
}

// syncFile flushes a single file's dirty pages to durable storage. fsync flushes
// the inode's cached writes regardless of the (read-only) open mode, so it commits
// data the post-processing library wrote and already closed.
func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

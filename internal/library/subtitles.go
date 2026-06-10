package library

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/torrentclaw/unarr/internal/agent"
)

// maxSubtitleBytes caps a downloaded subtitle (sane: even a long film SRT is
// a few hundred KB; this guards against a misbehaving upstream).
const maxSubtitleBytes = 10 << 20 // 10 MiB

var subtitleLangRe = regexp.MustCompile(`^[a-z]{2,3}$`)

var subtitleHTTPClient = &http.Client{Timeout: 30 * time.Second}

// FetchSubtitles downloads each requested subtitle (from our proxy URL, already
// charset-fixed WebVTT) and writes it as a sidecar next to the media file:
// `<basename>.<lang>.vtt`. Returns the IDs successfully written (or already
// present) and the ones that failed (with a short reason) so the web can mark
// them errored. Safety mirrors DeleteFiles: the media file must resolve within a
// configured scan path before we write beside it.
func FetchSubtitles(reqs []agent.SubtitleFetchRequest, scanPaths []string) (done []int, failed []agent.SubtitleFetchError) {
	// Resolve scan paths through symlinks too, so a symlinked root (e.g. the
	// docker bind-mount /downloads → /mnt/nas/peliculas) still matches a media
	// path that EvalSymlinks resolved to the real target. Mirrors the containment
	// check used for the resolved media path below.
	safe := make([]string, 0, len(scanPaths))
	for _, sp := range scanPaths {
		if !filepath.IsAbs(sp) {
			log.Printf("library: ignoring non-absolute scan path: %q", sp)
			continue
		}
		if real, err := filepath.EvalSymlinks(sp); err == nil {
			safe = append(safe, real)
		} else {
			safe = append(safe, filepath.Clean(sp))
		}
	}
	if len(safe) == 0 {
		log.Printf("library: no valid scan paths — refusing to write subtitle sidecars")
		for _, r := range reqs {
			failed = append(failed, agent.SubtitleFetchError{ID: r.ID, Error: "no valid scan paths"})
		}
		return nil, failed
	}

	for _, r := range reqs {
		if err := fetchSubtitleOne(r, safe); err != nil {
			log.Printf("library: subtitle fetch %d (%q): %v", r.ID, r.FilePath, err)
			msg := err.Error()
			if len(msg) > 480 {
				msg = msg[:480]
			}
			failed = append(failed, agent.SubtitleFetchError{ID: r.ID, Error: msg})
			continue
		}
		log.Printf("library: wrote subtitle sidecar for item %d (%s)", r.ID, r.Lang)
		done = append(done, r.ID)
	}
	return done, failed
}

func fetchSubtitleOne(r agent.SubtitleFetchRequest, scanPaths []string) error {
	if !filepath.IsAbs(r.FilePath) {
		return fmt.Errorf("path is not absolute: %q", r.FilePath)
	}
	lang := strings.ToLower(strings.TrimSpace(r.Lang))
	if !subtitleLangRe.MatchString(lang) {
		return fmt.Errorf("invalid language %q", r.Lang)
	}

	// Resolve the media file (symlinks too) and confine it to a scan path.
	real, err := filepath.EvalSymlinks(filepath.Clean(r.FilePath))
	if err != nil {
		return fmt.Errorf("media file unreachable: %w", err)
	}
	if !isWithinScanPaths(real, scanPaths) {
		return fmt.Errorf("path %q is outside all scan paths", real)
	}

	ext := filepath.Ext(real)
	sidecar := strings.TrimSuffix(real, ext) + "." + lang + ".vtt"
	if _, statErr := os.Stat(sidecar); statErr == nil {
		return nil // already present — idempotent success
	}

	data, err := downloadSubtitle(r.URL)
	if err != nil {
		return err
	}

	// Write atomically: temp in the same dir, then rename. Clean up any stale
	// .tmp from a prior crash first, and on every failure path, so a partial
	// write (disk full, killed) never lingers.
	tmp := sidecar + ".tmp"
	_ = os.Remove(tmp)
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write temp sidecar: %w", err)
	}
	if err := os.Rename(tmp, sidecar); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename sidecar: %w", err)
	}
	return nil
}

func downloadSubtitle(url string) ([]byte, error) {
	// Our proxy URL is always HTTPS. Restrict to https (allow http only for a
	// local dev server) so a tampered sync response can't point the agent at an
	// internal/metadata host.
	if !strings.HasPrefix(url, "https://") &&
		!strings.HasPrefix(url, "http://localhost") &&
		!strings.HasPrefix(url, "http://127.0.0.1") {
		return nil, fmt.Errorf("subtitle url must be https")
	}
	resp, err := subtitleHTTPClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSubtitleBytes))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("empty subtitle")
	}
	return data, nil
}

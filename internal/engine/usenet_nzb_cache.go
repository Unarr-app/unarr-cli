package engine

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// loadOrFetchNzb returns the NZB for nzbID, from the resume cache when it holds
// THAT NZB, from the server otherwise.
//
// The cache file is named after the task, not the NZB, so on its own it cannot
// tell which NZB it holds: a task whose NZB choice changes between runs (a
// fixed picker, a re-dispatch with a new pre-resolved id) kept downloading the
// first one. The sidecar <task>.nzb.id records the id. A cache without one
// predates it and is not trusted — re-fetching costs one small download, and
// the segment progress still resumes, because the tracker matches the NZB by
// fingerprint rather than by file name.
//
// fromCache tells the caller the bytes came from disk: a cache that fails to
// parse (neither file is fsynced, so a crash can leave it empty) should be
// dropped with dropNzbCache and fetched again, not fail every retry.
func (u *UsenetDownloader) loadOrFetchNzb(ctx context.Context, shortID, resumeDir, taskID, nzbID string) (data []byte, fromCache bool, err error) {
	cachePath := filepath.Join(resumeDir, taskID+".nzb")
	idPath := cachePath + ".id"
	if id, err := os.ReadFile(idPath); err == nil && strings.TrimSpace(string(id)) == nzbID {
		if data, err := os.ReadFile(cachePath); err == nil {
			log.Printf("[%s] using cached NZB", shortID)
			return data, true, nil
		}
	}

	data, err = u.apiClient.DownloadNzb(ctx, nzbID)
	if err != nil {
		return nil, false, fmt.Errorf("download NZB: %w", err)
	}
	// Cache for future resume (best-effort — download still works without cache).
	// The id goes last: a crash in between leaves a cache that is not trusted.
	if mkErr := os.MkdirAll(resumeDir, 0o755); mkErr != nil {
		log.Printf("[%s] resume dir create failed: %v", shortID, mkErr)
		return data, false, nil
	}
	_ = os.Remove(idPath)
	if wErr := os.WriteFile(cachePath, data, 0o644); wErr != nil {
		log.Printf("[%s] NZB cache write failed: %v", shortID, wErr)
		return data, false, nil
	}
	if wErr := os.WriteFile(idPath, []byte(nzbID), 0o644); wErr != nil {
		log.Printf("[%s] NZB cache id write failed: %v", shortID, wErr)
	}
	return data, false, nil
}

// dropNzbCache forgets the cached NZB of a task (the id first, so a crash in
// between leaves an untrusted cache rather than a trusted broken one).
func dropNzbCache(resumeDir, taskID string) {
	cachePath := filepath.Join(resumeDir, taskID+".nzb")
	_ = os.Remove(cachePath + ".id")
	_ = os.Remove(cachePath)
}

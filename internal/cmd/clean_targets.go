package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

// What `unarr clean` considers removable, and how much of it is on disk.
// Split from clean.go so the command keeps to driving the flow (confirm,
// delete, report) while the rules about WHAT to remove live here.

type cleanTarget struct {
	path        string
	description string
	isDir       bool
	isGlob      bool
}

// logCleanTargets lists every log file `clean` removes, live files and rings.
//
// It must cover exactly what the janitor supervises (daemonLogPaths), or a file
// one of them knows about and the other does not is a file that survives a
// clean and grows without a ceiling. The boot log needs its OWN entries: it is
// not caught by the `unarr.log.*` glob, which matches unarr.log.1 but never
// unarr.boot.log.
//
// The rings are globbed rather than listed from log_max_files, so a ring left
// behind by an older/larger setting is still swept — the point of `clean` is
// that nothing survives it.
func logCleanTargets(dataDir string) []cleanTarget {
	return []cleanTarget{
		{filepath.Join(dataDir, logFileName), "daemon log", false, false},
		{filepath.Join(dataDir, logFileName+".*"), "rotated daemon log", false, true},
		{filepath.Join(dataDir, bootLogFileName), "daemon startup log", false, false},
		{filepath.Join(dataDir, bootLogFileName+".*"), "rotated daemon startup log", false, true},
		{filepath.Join(dataDir, errLogFileName), "daemon error log", false, false},
		{filepath.Join(dataDir, errLogFileName+".*"), "rotated daemon error log", false, true},
	}
}

// foundEntry represents a file or directory found during scanning.
type foundEntry struct {
	path  string
	desc  string
	size  int64
	isDir bool
}

// resolveHLSCacheDir returns the HLS cache location the daemon would use:
// hls_cache.dir when set, otherwise the platform default. Reads the config
// directly because `clean` runs with the daemon stopped and cannot ask it.
// An unreadable config falls back to the default rather than failing — the
// default is where the cache is in every case except an explicit override.
func resolveHLSCacheDir() string {
	if cfg, err := config.Load(""); err == nil && cfg.Download.HLSCache.Dir != "" {
		return cfg.Download.HLSCache.Dir
	}
	return config.HLSCacheDir()
}

// cleanTargetsFor builds the removal list for a set of flags. Resume and
// replaced-file backups are not here: those are age-filtered and handled
// separately by the caller.
func cleanTargetsFor(o cleanOpts, dataDir string) []cleanTarget {
	tmpDir := os.TempDir()

	// With --all, remove the entire data directory (includes logs, state, resume).
	// Without --all, target individual files + stale resume files only.
	var targets []cleanTarget
	if o.all {
		targets = []cleanTarget{
			{dataDir, "data directory", true, false},
		}
	} else {
		targets = append(logCleanTargets(dataDir),
			cleanTarget{filepath.Join(dataDir, "daemon.state.json"), "daemon state", false, false},
			cleanTarget{filepath.Join(dataDir, "daemon.state.json.tmp"), "daemon state temp", false, false},
		)
	}

	// Temp targets apply regardless of --all
	targets = append(targets,
		cleanTarget{filepath.Join(tmpDir, "unarr-stream"), "stream temp data", true, false},
		cleanTarget{filepath.Join(tmpDir, "unarr-download-*.tmp"), "upgrade temp files", false, true},
		cleanTarget{config.FilePath() + ".tmp", "config temp", false, false},
	)

	// The HLS cache is opt-in: it is a performance store, and wiping it costs
	// a full re-encode on the next play of every title. --all implies it, on
	// the reading that --all means "leave nothing behind". It also lives
	// outside the data dir, so --all would otherwise miss it.
	if o.hlsCache || o.all {
		targets = append(targets,
			cleanTarget{resolveHLSCacheDir(), "transcoded HLS segments", true, false})
	}
	return targets
}

// scanAgedExtras collects the two target sets that are filtered by age rather
// than by path: usenet resume files, and the backups organize's replaceFile
// keeps under replaced/ on every library upgrade (nothing else ever reaped
// those, so the directory grew without bound). Both are skipped under --all,
// which already removes them via the whole-dataDir target.
//
// resumeSkipped reports how many recent resume files were deliberately kept,
// so the command can tell the user download progress was preserved.
func scanAgedExtras(dataDir string, all bool) (found []foundEntry, resumeSkipped int) {
	if all {
		return nil, 0
	}
	found, resumeSkipped = scanResumeFiles(filepath.Join(dataDir, "resume"), false)
	return append(found, scanReplacedFiles(filepath.Join(dataDir, "replaced"), false)...), resumeSkipped
}

// scanCleanTargets resolves each target to what actually exists on disk,
// returning the entries to show plus their file count and total size. Missing
// targets are skipped silently — the list describes everything unarr can
// create, and a given install will only ever have some of it.
func scanCleanTargets(targets []cleanTarget) (found []foundEntry, totalFiles int, totalBytes int64) {
	for _, t := range targets {
		if t.isGlob {
			for _, m := range globMatches(t.path) {
				size := fileSize(m)
				totalFiles++
				totalBytes += size
				found = append(found, foundEntry{m, t.description, size, false})
			}
			continue
		}
		info, err := os.Stat(t.path)
		if err != nil {
			continue
		}
		if !t.isDir {
			totalFiles++
			totalBytes += info.Size()
			found = append(found, foundEntry{t.path, t.description, info.Size(), false})
			continue
		}
		files, bytes := dirStats(t.path)
		if files == 0 {
			continue
		}
		totalFiles += files
		totalBytes += bytes
		found = append(found, foundEntry{
			path:  t.path + "/",
			desc:  fmt.Sprintf("%s (%d files)", t.description, files),
			size:  bytes,
			isDir: true,
		})
	}
	return found, totalFiles, totalBytes
}

// globMatches returns the paths matching a glob target, or nothing when the
// pattern is malformed — a bad pattern means no such files, not an error worth
// aborting a cleanup for.
func globMatches(pattern string) []string {
	matches, _ := filepath.Glob(pattern)
	return matches
}

package engine

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// ErrNoMatchingNzb: the indexers have the title, but no NZB of the release the
// task names. Usenet is then unavailable on purpose, and the reason is carried
// into the task's error instead of a bare "no download method available".
var ErrNoMatchingNzb = errors.New("usenet: no NZB matches the release")

// nzbMaxSizeRatio bounds how far an NZB's size may stray from the release the
// user picked, either way. Encodes of the same resolution legitimately differ
// (x265 vs x264, audio tracks), a remux or a sample does not belong: 4× keeps
// the first and rejects the second.
const nzbMaxSizeRatio = 4.0

// nzbSearchLimit is how many results the picker sees. Matching is strict, so
// the right release must be among them: 10 (the old value) often held none on
// a popular title. The server caps it at 100.
const nzbSearchLimit = 50

// nzbTarget is the release a usenet download has to reproduce. On 2026-09-23
// a stalled 1.5 GB 1080p torrent on a NAS fell back to a 58.8 GB 2160p remux,
// because the search took the largest result.
type nzbTarget struct {
	resolution string // "2160p" | "1080p" | "720p" | "480p", "" = unknown
	size       int64  // bytes, 0 = unknown
}

func (t nzbTarget) String() string {
	res, size := t.resolution, "size unknown"
	if res == "" {
		res = "resolution unknown"
	}
	if t.size > 0 {
		size = formatBytes(t.size)
	}
	return fmt.Sprintf("%s, %s", res, size)
}

// nzbTargetFor derives the target from the task: the resolution its title
// states, and the size of the release — the torrent's from its metadata, else
// the debrid file's, else what the server knows about the torrent. The last one
// is what a dead magnet has: it never yields metadata, and without a size the
// picker took a 25.8 GB 1080p remux for a 1.5 GB file (local repro, same day).
//
// The configured preferred quality stands in for the resolution only when
// NOTHING describes the release: a known size already identifies it, and a
// preference ("2160p") must not override what the user actually picked. When
// it does stand in, it filters like a title resolution would — deliberately:
// matching is strict, so a user preferring 2160p gets no 1080p NZB for a task
// whose title and size say nothing.
func nzbTargetFor(task *Task, preferredQuality string) nzbTarget {
	task.mu.RLock()
	size := task.TotalBytes
	task.mu.RUnlock()
	if size <= 0 {
		size = task.DirectFileSize
	}
	if size <= 0 {
		size = task.ReleaseSize
	}
	res := releaseResolution(task.Title)
	if res == "" && size <= 0 {
		res = preferredQuality
	}
	return nzbTarget{resolution: res, size: size}
}

var (
	// A resolution token standing on its own: "1080p" in "Film.1080p.x264",
	// "[1080P]", "1080p60" or "BD1080p", not in "1720p". Explicit NNNNp tokens
	// and WxH frame sizes are preferred over 4K/UHD tags, which also appear as
	// edition labels next to a real resolution ("UHD.BluRay.1080p").
	pixelsRe = regexp.MustCompile(`(?i)(?:^|[^0-9])(2160|1080|720|480)[pi](?:[^a-z]|$)`)
	frameRe  = regexp.MustCompile(`(?i)(?:^|[^0-9])(?:3840|1920|1280|854|640)x(2160|1080|720|480)(?:[^0-9]|$)`)
	uhdRe    = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(4k|uhd)(?:[^a-z0-9]|$)`)
)

// releaseResolution reads the resolution a release title states, "" if none.
func releaseResolution(title string) string {
	if m := pixelsRe.FindStringSubmatch(title); m != nil {
		return m[1] + "p"
	}
	if m := frameRe.FindStringSubmatch(title); m != nil {
		return m[1] + "p"
	}
	if uhdRe.MatchString(title) {
		return "2160p"
	}
	return ""
}

// normalizeResolution maps the spellings the server and config use onto
// releaseResolution's values ("4K" → "2160p", "1080P" → "1080p").
func normalizeResolution(s string) string {
	switch s = strings.ToLower(strings.TrimSpace(s)); s {
	case "2160p", "1080p", "720p", "480p":
		return s
	case "4k", "uhd":
		return "2160p"
	}
	return ""
}

// nzbResolution is a result's resolution: the server's parse when it sent
// one, the local read of the title otherwise.
func nzbResolution(r *agent.NzbSearchResult) string {
	if res := normalizeResolution(r.Parsed.Resolution); res != "" {
		return res
	}
	return releaseResolution(r.Title)
}

// pickNzb returns the search result that best reproduces target, or nil when
// none is close enough — and nil means no usenet. Every task names a release
// (a web task carries an info hash or an NZB id; the user picked it), so usenet
// only ever stands in for THAT release: the wrong one is worse than none,
// whether usenet runs after another method failed or first because torrent was
// merely unavailable (VPN kill-switch down, debrid not cached).
//
//   - A known resolution must match exactly; a result that does not state one
//     is not trusted to.
//   - A known size must be within nzbMaxSizeRatio; among those the closest
//     (log-scale) wins, then the most grabbed.
//   - With nothing known about the target, the old rule stands: largest, then
//     most grabbed.
func pickNzb(results []agent.NzbSearchResult, target nzbTarget) *agent.NzbSearchResult {
	var best *agent.NzbSearchResult
	bestDist := math.Inf(1)
	for i := range results {
		r := &results[i]
		if target.resolution != "" && nzbResolution(r) != target.resolution {
			continue
		}
		dist := 0.0
		if target.size > 0 {
			if r.Size <= 0 {
				continue
			}
			dist = math.Abs(math.Log(float64(r.Size) / float64(target.size)))
			if dist > math.Log(nzbMaxSizeRatio) {
				continue
			}
		}
		if best == nil || betterNzb(r, dist, best, bestDist, target.size > 0) {
			best, bestDist = r, dist
		}
	}
	return best
}

func betterNzb(r *agent.NzbSearchResult, dist float64, best *agent.NzbSearchResult, bestDist float64, bySize bool) bool {
	if bySize {
		if dist != bestDist {
			return dist < bestDist
		}
		return r.Grabs > best.Grabs
	}
	if r.Size != best.Size {
		return r.Size > best.Size
	}
	return r.Grabs > best.Grabs
}

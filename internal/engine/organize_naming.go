package engine

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/naming"
)

// scheme returns the naming templates organize renders with: the configured
// ones, or the default layout when none was set (zero OrganizeConfig in tests,
// a manager built before naming existed).
func (cfg OrganizeConfig) scheme() naming.Scheme {
	if cfg.Naming.Movie == nil || cfg.Naming.Series == nil {
		return naming.Default()
	}
	return cfg.Naming
}

// namingVars collects the task's metadata as template values. Season/episode
// are left to the caller: they depend on what the file itself turned out to be.
func namingVars(task *Task, title, year string) naming.Vars {
	v := naming.Vars{
		Title:        sanitizedOrUnknown(title),
		Year:         year,
		ImdbID:       task.IMDbID,
		Collection:   task.CollectionName,
		EpisodeTitle: task.EpisodeTitle,
		VideoFormat:  videoFormat(task.Title),
	}
	if task.CollectionName != "" {
		v.Collection = sanitizedOrUnknown(task.CollectionName)
	}
	if task.TmdbID > 0 {
		v.TmdbID = strconv.Itoa(task.TmdbID)
	}
	if task.TvdbID > 0 {
		v.TvdbID = strconv.Itoa(task.TvdbID)
	}
	return v
}

// sanitizedOrUnknown keeps sanitizePath's guarantee for a name that is set but
// sanitizes to nothing ("???"): it becomes "Unknown" rather than vanishing —
// a title or collection folder must never drop out of the path.
func sanitizedOrUnknown(name string) string {
	if naming.Sanitize(name) == "" {
		return "Unknown"
	}
	return name
}

// contentYear is the server's (TMDB) year, "" when unknown — no guessing from
// the release name.
func contentYear(task *Task) string {
	if task.ContentYear != nil && *task.ContentYear > 0 {
		return strconv.Itoa(*task.ContentYear)
	}
	return ""
}

// renderDest renders tpl under root. destFileName is "" when the template can't
// name the file (a value it needs is unknown) — organize then keeps the
// download's own file name, as it always did for an unnumbered episode.
func renderDest(tpl *naming.Template, root string, v naming.Vars, ext string) (destDir, destFileName string) {
	r := tpl.Render(v)
	destDir = filepath.Join(append([]string{root}, r.Dirs...)...)
	if r.FileOK {
		destFileName = naming.FitFileName(r.File, ext)
	}
	return destDir, destFileName
}

// videoFormat is the release's resolution label ("1080p", "2160p") for {vf}.
func videoFormat(title string) string {
	m := strings.ToLower(resTagRegex.FindString(title))
	if m == "4k" {
		return "2160p"
	}
	return m
}

// commonDir is the deepest directory containing every dir in dirs.
func commonDir(dirs []string) string {
	if len(dirs) == 0 {
		return ""
	}
	common := dirs[0]
	for _, d := range dirs[1:] {
		for !isWithinDir(common, d) && common != filepath.Dir(common) {
			common = filepath.Dir(common)
		}
	}
	return common
}

package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/doctor"
)

// The library checks (plan A1.5). Doctor tested download_dir and stopped there,
// but a download does not finish in download_dir — organize MOVES it into
// movies_dir / tv_shows_dir, and those were never looked at.
//
// The failure that motivates this is not "no write permission", which fails
// loudly and early. It is the NFS root_squash / SMB uid-mapping case that
// engine.makeReadable already knows about: the chmod REPORTS SUCCESS and the
// file is still not openable by this uid. The download completes, organize
// moves it, and playback fails later with an opaque permission error pointing
// at nothing. So the probe here is not "can I write" — it is write, chmod,
// then REOPEN, which is the only step that catches it.
//
// The free-space check is separate from the download-dir one on purpose: a
// library on a NAS mount and a download dir on the local SSD are different
// filesystems, and the existing check measures the wrong one.

// libraryProbePrefix names the temporary files these checks create, so a probe
// interrupted by a kill is recognisable as ours and not mistaken for media.
const libraryProbePrefix = ".unarr-doctor-probe-"

// lowDiskWarnBytes is the per-library threshold. Deliberately larger than the
// download dir's: this is where finished media lands and stays.
const lowDiskWarnBytes = 10 << 30 // 10 GiB

func doctorLibrarySpecs(cfg *config.Config) []doctor.Spec {
	return []doctor.Spec{
		{
			Group:  "Library",
			Name:   "Library directories",
			Remedy: "fix ownership/permissions on the directory (on NFS check root_squash, on SMB the uid mapping)",
			Fn:     func() (string, error) { return libraryDirsResult(libraryDirs(cfg)) },
		},
		{
			Group: "Library",
			Name:  "Library free space",
			Fn:    func() (string, error) { return libraryFreeSpaceResult(libraryDirs(cfg)) },
		},
	}
}

// libraryDir is one configured destination, named by the key that set it so a
// finding points at the line the user has to edit.
type libraryDir struct {
	key  string
	path string
}

// libraryDirs is the configured set, deduplicated. Pointing movies_dir and
// tv_shows_dir at the same directory is a normal setup, and probing it twice
// would report every fault twice.
func libraryDirs(cfg *config.Config) []libraryDir {
	seen := map[string]bool{}
	var out []libraryDir
	for _, d := range []libraryDir{
		{"organize.movies_dir", cfg.Organize.MoviesDir},
		{"organize.tv_shows_dir", cfg.Organize.TVShowsDir},
	} {
		p := strings.TrimSpace(d.path)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, libraryDir{key: d.key, path: p})
	}
	return out
}

func libraryDirsResult(dirs []libraryDir) (string, error) {
	if len(dirs) == 0 {
		return "not configured (organize leaves files in the download dir)", nil
	}
	var problems []string
	for _, d := range dirs {
		if msg := probeLibraryDir(d.path); msg != "" {
			problems = append(problems, d.key+": "+msg)
		}
	}
	if len(problems) > 0 {
		return strings.Join(problems, "; "), fmt.Errorf("%d library directory problem(s)", len(problems))
	}
	return fmt.Sprintf("%d directory(ies) writable, and files stay readable after chmod", len(dirs)), nil
}

// probeLibraryDir returns "" when the directory is usable, or the reason it is
// not. The steps run in the order organize itself hits them.
func probeLibraryDir(dir string) string {
	fi, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return "does not exist"
	}
	if err != nil {
		return "unreadable: " + err.Error()
	}
	if !fi.IsDir() {
		return "not a directory"
	}

	f, err := os.CreateTemp(dir, libraryProbePrefix+"*")
	if err != nil {
		return "not writable: " + err.Error()
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err := f.Write([]byte("unarr")); err != nil {
		f.Close()
		return "write failed: " + err.Error()
	}
	if err := f.Close(); err != nil {
		return "close failed: " + err.Error()
	}
	return probeChmodThenReopen(name)
}

// probeChmodThenReopen is the whole point of this check. os.Chmod returning nil
// is NOT evidence the mode took effect: on an NFS export with root_squash, or
// an SMB share whose uid mapping does not include this user, the call succeeds
// and the file is still unopenable. engine.makeReadable found this the hard
// way; running the same two steps here means doctor reports it before a
// download does, instead of after.
func probeChmodThenReopen(path string) string {
	if err := os.Chmod(path, 0o644); err != nil {
		return "chmod failed: " + err.Error()
	}
	g, err := os.Open(path)
	if err != nil {
		return "a file written here could not be reopened after chmod " +
			"(NFS root_squash or SMB uid mapping): " + err.Error()
	}
	g.Close()
	return ""
}

func libraryFreeSpaceResult(dirs []libraryDir) (string, error) {
	if len(dirs) == 0 {
		return "not configured", nil
	}
	var parts []string
	low := false
	for _, d := range dirs {
		// A directory that is not there has no free space to report, and the
		// check above already said so in the words the user can act on.
		// Repeating it here as "unavailable" was noise that also dragged the
		// raw path — and therefore the account name — into doctor.json.
		if fi, err := os.Stat(d.path); err != nil || !fi.IsDir() {
			continue
		}
		free, _, err := agent.DiskInfoBounded(d.path)
		if err != nil {
			// Named by CONFIG KEY, never by path: this string is published in
			// `doctor --json` and embedded in every support bundle.
			parts = append(parts, d.key+": unreadable")
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: %.1f GB free", filepath.Base(d.path), float64(free)/(1<<30)))
		if free < lowDiskWarnBytes {
			low = true
		}
	}
	if len(parts) == 0 {
		return "no existing library directory to measure", nil
	}
	msg := strings.Join(parts, ", ")
	// WARN, never FAIL. A full library disk stops new downloads landing; it does
	// not break the agent, and a red doctor for "you should buy a bigger disk"
	// trains people to ignore red.
	if low {
		return "!" + msg + " — below 10 GB", nil
	}
	return msg, nil
}

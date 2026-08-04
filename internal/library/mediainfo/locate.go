package mediainfo

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// mediaTool describes one binary of the ffmpeg family in every place it can
// legitimately live. ffmpeg and ffprobe are searched identically — the only
// differences are the name, the env override and the cache path — so the
// search itself is written once here and the two Resolve* functions layer the
// auto-download and the operator-facing error message on top.
type mediaTool struct {
	name      string                 // "ffmpeg" / "ffprobe"
	envVar    string                 // FFMPEG_PATH / FFPROBE_PATH
	cachePath func() (string, error) // where a previously downloaded copy lands
}

var (
	ffmpegTool  = mediaTool{name: "ffmpeg", envVar: "FFMPEG_PATH", cachePath: FFmpegCachePath}
	ffprobeTool = mediaTool{name: "ffprobe", envVar: "FFPROBE_PATH", cachePath: FFprobeCachePath}
)

// LocateFFmpeg finds an ffmpeg that is ALREADY on this host and never downloads
// one. That distinction is the whole point of the split from ResolveFFmpeg:
// `unarr doctor` has to answer "is ffmpeg installed?" in milliseconds, and
// ResolveFFmpeg answers it by fetching ~50 MB — which would turn an interactive
// diagnostic into a silent installer and report "present" for a host that had
// nothing a second earlier.
//
// Search order (same as ResolveFFmpeg, minus the download):
//  1. explicit path (--ffmpeg flag / [library] ffmpeg_path)
//  2. FFMPEG_PATH env var
//  3. "ffmpeg" on PATH
//  4. adjacent to the running executable (release tarballs bundle it there)
//  5. a copy downloaded by an earlier run, in the unarr cache dir
func LocateFFmpeg(explicit string) (string, bool) { return ffmpegTool.locate(explicit) }

// LocateFFprobe is LocateFFmpeg for ffprobe. See its doc comment.
func LocateFFprobe(explicit string) (string, bool) { return ffprobeTool.locate(explicit) }

func (t mediaTool) locate(explicit string) (string, bool) {
	// An explicit path is a decision, not a hint: if the operator named a
	// binary and it is not there, falling through to PATH would silently run a
	// different build than the one they configured.
	if explicit != "" {
		if pathExists(explicit) {
			return explicit, true
		}
		return "", false
	}
	if env := os.Getenv(t.envVar); env != "" && pathExists(env) {
		return env, true
	}
	if p, err := exec.LookPath(t.name); err == nil {
		return p, true
	}
	if p, ok := t.adjacentToExecutable(); ok {
		return p, true
	}
	if cached, err := t.cachePath(); err == nil && pathExists(cached) {
		return cached, true
	}
	return "", false
}

func (t mediaTool) adjacentToExecutable() (string, bool) {
	exePath, err := os.Executable()
	if err != nil {
		return "", false
	}
	name := t.name
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	p := filepath.Join(filepath.Dir(exePath), name)
	return p, pathExists(p)
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

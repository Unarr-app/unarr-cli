package mediainfo

// Spawn + parse halves of the keyframe index, split out of indexKeyframes so
// each piece stays small enough to read: budget selection, running ffprobe (with
// the timeout-vs-failure classification the caller depends on), and turning its
// CSV into timestamps.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/winproc"
)

// indexBudget picks the deadline for one index run. A window reads a bounded
// slice, so it must NOT inherit the whole-file budget: on the playback path its
// whole point is answering fast, or failing fast enough to leave time for a
// fallback.
func indexBudget(mediaPath, readInterval string) time.Duration {
	if readInterval != "" {
		return copyKeyframeWindowTimeout
	}
	return keyframeIndexTimeout(mediaPath)
}

// runKeyframeProbe runs the demux-only ffprobe pass and returns its raw CSV.
//
// budget is only used to describe a timeout in the error; pass 0 when the
// deadline came from the caller. A deadline kill is reported as
// ErrKeyframeIndexTimeout rather than the bare "signal: killed ()" — ffprobe
// writes nothing to stderr when killed, so the raw message reads like file
// corruption and sent past debugging down the wrong path.
func runKeyframeProbe(ctx context.Context, ffprobePath, mediaPath, readInterval string, budget time.Duration) (*bytes.Buffer, error) {
	args := []string{"-v", "error", "-select_streams", "v:0"}
	if readInterval != "" {
		args = append(args, "-read_intervals", readInterval)
	}
	args = append(args,
		"-show_entries", "packet=pts_time,flags",
		"-of", "csv=p=0",
		mediaPath,
	)
	cmd := exec.CommandContext(ctx, ffprobePath, args...)
	winproc.HideWindow(cmd)
	// Same treatment as the subtitle/trickplay passes: a full sequential demux of
	// a multi-GB file yields disk bandwidth to live playback (IDLE I/O) and dies
	// with the agent instead of being reparented to init, where it would run its
	// whole budget as an orphan (hardenCmd).
	hardenCmd(cmd)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("keyframe index start: %w", err)
	}
	setIdleIOPriority(cmd.Process.Pid)
	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		killed := errors.As(err, &exitErr) && !exitErr.ProcessState.Exited()
		if killed && ctx.Err() != nil {
			if budget > 0 {
				return nil, fmt.Errorf("%w after %s (%s)", ErrKeyframeIndexTimeout, budget, filepath.Base(mediaPath))
			}
			return nil, fmt.Errorf("%w: caller deadline (%s)", ErrKeyframeIndexTimeout, filepath.Base(mediaPath))
		}
		return nil, fmt.Errorf("keyframe index: %w (%s)", err, strings.TrimSpace(errBuf.String()))
	}
	return &out, nil
}

// parseKeyframeCSV extracts keyframe presentation timestamps from ffprobe's
// packet CSV. Each line is "pts_time,flags", e.g. "6.006000,K__"; the flags
// field carries "K" for a keyframe (RAP). Malformed lines are skipped rather
// than failing the whole index — ffprobe emits every packet, and one unparseable
// row should not cost the file its seekability.
func parseKeyframeCSV(out *bytes.Buffer) ([]float64, error) {
	kfs := make([]float64, 0, 1024)
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		comma := strings.IndexByte(line, ',')
		if comma < 0 {
			continue
		}
		ptsStr := strings.TrimSpace(line[:comma])
		if !strings.Contains(line[comma+1:], "K") || ptsStr == "" || ptsStr == "N/A" {
			continue
		}
		v, err := strconv.ParseFloat(ptsStr, 64)
		if err != nil || v < 0 {
			continue
		}
		kfs = append(kfs, v)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("keyframe index scan: %w", err)
	}
	return kfs, nil
}

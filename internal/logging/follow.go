package logging

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// pollInterval is how often a follower looks for new bytes. Polling rather than
// inotify/ReadDirectoryChangesW keeps this one implementation working on Linux,
// macOS, Windows and every network share a NAS install might put the data dir
// on — and a quarter second is imperceptible when reading a log.
const pollInterval = 250 * time.Millisecond

// Follow prints the matching tail of the log and then streams new lines as they
// land, re-opening the file when rotation (ours or an external logrotate)
// replaces it underneath. Returns nil when ctx is cancelled — a user pressing
// Ctrl-C has not hit an error.
func Follow(ctx context.Context, q Query, w io.Writer) error {
	// How much of the file Print is about to consume, sampled BEFORE it runs.
	// The follower then resumes from exactly there, which is what stops the two
	// halves of this function from disagreeing about the same file.
	//
	// The bug this replaces: Print ran first, then the follower opened the file
	// and seeked to its END. A log CREATED in the window between those two
	// steps was therefore never printed (Print saw no file) and never streamed
	// (the seek skipped past it) — the first lines a daemon ever wrote were lost
	// outright. Sampling first also makes the ordering honest in the other
	// direction: if the file grows between the sample and Print, the worst case
	// is a line shown twice, and a log viewer that repeats itself is a far
	// smaller failure than one that silently drops the beginning.
	resumeAt := fileSize(q.Path)
	if err := Print(q, w); err != nil {
		return err
	}
	m, err := q.compile()
	if err != nil {
		return err
	}
	f := &follower{path: q.Path, format: q.Format, out: w, filter: lineFilter{m: m}}
	defer f.close()
	f.openAt(resumeAt)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(pollInterval):
		}
		if err := f.step(); err != nil {
			return err
		}
	}
}

// follower holds the state of one `logs --follow` session: the open file, how
// far it has been read, and the partial last line still waiting for its
// newline.
type follower struct {
	path   string
	format Format
	out    io.Writer
	filter lineFilter

	f    *os.File
	rd   *bufio.Reader
	info os.FileInfo
	frag string
	// offset is how far into the file we have consumed. Compared against the
	// file's current size to spot a copy-truncate: a rename changes the inode
	// (os.SameFile catches it), but truncate-in-place only makes the file
	// shorter than what we already read.
	offset int64
}

// fileSize is the file's current length, or 0 when it does not exist yet. A
// missing log is the ordinary case here — the daemon may not have started.
func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// openAt attaches to the live file and resumes reading at `at` — the point
// Print stopped, so nothing is shown twice and nothing is skipped. A file that
// does not exist yet is not an error; step() picks it up when it appears.
//
// `at` past the current end (the file was truncated between the sample and
// here) is self-correcting: reads return EOF until it grows again, and
// reopenIfReplaced sees info.Size() < offset and re-attaches from the start.
func (fo *follower) openAt(at int64) {
	f, err := os.Open(fo.path)
	if err != nil {
		return
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return
	}
	if _, err := f.Seek(at, io.SeekStart); err != nil {
		f.Close()
		return
	}
	fo.attach(f, info, at)
}

// openAtStart attaches to a file we have not read at all — a fresh log after
// rotation, or one that has just been truncated.
func (fo *follower) openAtStart() {
	f, err := os.Open(fo.path)
	if err != nil {
		return
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return
	}
	fo.attach(f, info, 0)
}

// attach replaces the current handle, dropping any half-read line with it.
func (fo *follower) attach(f *os.File, info os.FileInfo, at int64) {
	fo.close()
	fo.f, fo.rd, fo.info, fo.frag, fo.offset = f, bufio.NewReader(f), info, "", at
}

func (fo *follower) close() {
	if fo.f != nil {
		fo.f.Close()
		fo.f = nil
	}
}

// step drains whatever arrived since the last tick, then reacts to the file
// being rotated, truncated or created.
func (fo *follower) step() error {
	if fo.f != nil {
		if err := fo.drain(); err != nil {
			return err
		}
	}
	return fo.reopenIfReplaced()
}

// drain emits every complete line available right now, remembering a trailing
// partial line so a record that is still being written is printed once, whole.
func (fo *follower) drain() error {
	for {
		chunk, err := fo.rd.ReadString('\n')
		if chunk != "" {
			fo.frag += chunk
		}
		if err != nil {
			return fo.pause(err)
		}
		line := strings.TrimRight(fo.frag, "\r\n")
		fo.frag = ""
		if err := fo.emit(line); err != nil {
			return err
		}
	}
}

// pause ends a drain. io.EOF only means "nothing more yet", so it is not a
// failure — and since the bufio buffer is empty at EOF, the descriptor's
// position is exactly what has been consumed, which is the offset
// reopenIfReplaced compares a shrunken file against.
func (fo *follower) pause(err error) error {
	if !errors.Is(err, io.EOF) {
		return fmt.Errorf("read log file: %w", err)
	}
	if pos, serr := fo.f.Seek(0, io.SeekCurrent); serr == nil {
		fo.offset = pos
	}
	return nil
}

// emit prints one complete line, when the query keeps it.
func (fo *follower) emit(line string) error {
	e, ok := fo.filter.accept(line)
	if !ok {
		return nil
	}
	_, err := fmt.Fprintln(fo.out, fo.format.Render(e))
	return err
}

// reopenIfReplaced re-attaches when the path now points at a different file
// (rotation) or the same file got shorter (truncation), and picks the file up
// for the first time when it did not exist before.
func (fo *follower) reopenIfReplaced() error {
	info, err := os.Stat(fo.path)
	if err != nil {
		// Gone for the moment — mid-rotation, or not created yet. Keep the old
		// handle: the last lines of the rotated file are still worth printing.
		return nil
	}
	if fo.f == nil {
		fo.openAtStart()
		return nil
	}
	if !os.SameFile(fo.info, info) || info.Size() < fo.offset {
		// Rotated (new inode) or truncated in place. Drain the old handle one
		// last time so nothing written just before the swap is lost.
		if derr := fo.drain(); derr != nil {
			return derr
		}
		fo.openAtStart()
		return nil
	}
	fo.info = info
	return nil
}

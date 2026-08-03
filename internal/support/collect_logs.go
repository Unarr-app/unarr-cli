package support

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"github.com/Unarr-app/unarr-cli/internal/logging"
)

// LogPaths names the daemon's three log files. They are passed in rather than
// resolved here because internal/cmd owns both the names and the data-dir
// lookup (see daemon_logfiles.go) — duplicating the literals would let the two
// drift, and a bundle reading the wrong file is worse than one reading none.
// An empty entry is reported as absent.
type LogPaths struct {
	// Daemon is unarr.log, the running daemon's own output.
	Daemon string
	// Err is unarr.err.log — on the macOS installs whose plist split stderr,
	// this is where most of the log actually ends up.
	Err string
	// Boot is unarr.boot.log, held by the SERVICE LAUNCHER rather than the
	// daemon. It is the only place a daemon that never started leaves a trace,
	// which makes it the first file to read when there is no daemon log at all.
	Boot string
	// MaxFiles is how many rotated siblings (unarr.log.1 …) to walk back
	// through, mirroring [daemon] log_max_files. Rotation means "the last 500
	// lines" legitimately spans two files.
	MaxFiles int
}

// errNoJournalLog is what a systemd host reports for the file-based sections.
// Not an error the user should act on — it is the platform working as designed,
// and saying so stops a reader from hunting for a file that cannot exist.
var errNoJournalLog = errors.New("absent on this host: the daemon runs under systemd and logs to the journal, not to a file (see unarr.log)")

// daemonLogText collects the daemon's recent output from whichever source this
// host actually has.
//
// The journal branch is not an optimisation: under systemd there is no
// unarr.log, and a stale one left behind by an earlier foreground `unarr up`
// would silently answer with output from a different run. `unarr logs` makes
// the same choice, through the same injected reader.
func daemonLogText(in Inputs) ([]byte, error) {
	if in.Journal != nil {
		var buf bytes.Buffer
		if err := in.Journal(&buf, in.lines()); err != nil {
			return nil, fmt.Errorf("read systemd journal: %w", err)
		}
		return withHeader("source: systemd journal (this host has no unarr.log)", buf.Bytes()), nil
	}
	return tailLog(in.Logs.Daemon, in.lines(), in.Logs.MaxFiles)
}

// errLogText collects unarr.err.log.
func errLogText(in Inputs) ([]byte, error) {
	if in.Journal != nil {
		return nil, errNoJournalLog
	}
	return tailLog(in.Logs.Err, in.lines(), in.Logs.MaxFiles)
}

// bootLogText collects unarr.boot.log. It is NOT read from the journal even
// when one exists: the boot log answers "did the launcher manage to start the
// process at all", and the journal cannot answer a question asked about a file.
func bootLogText(in Inputs) ([]byte, error) {
	if in.Journal != nil {
		return nil, errNoJournalLog
	}
	return tailLog(in.Logs.Boot, in.lines(), in.Logs.MaxFiles)
}

// tailLog returns the last n lines of a log file and its rotated siblings,
// verbatim.
//
// logging.Print with a zero MinLevel and the text format keeps every line
// exactly as the daemon wrote it (Format.Render returns Entry.Raw), and walks
// the rotation ring for us. Filtering here would be wrong twice over: severity
// in this log is inferred from the text, and the line a support bundle needs is
// often the unparseable one right before the crash.
func tailLog(path string, n, maxFiles int) ([]byte, error) {
	if path == "" {
		return nil, errors.New("absent: no path for this log on this platform")
	}
	if !ringExists(path, maxFiles) {
		return nil, fmt.Errorf("absent: %s does not exist (the daemon may never have run on this machine)", path)
	}
	var buf bytes.Buffer
	q := logging.Query{Path: path, Lines: n, Format: logging.FormatText, MaxFiles: maxFiles}
	if err := logging.Print(q, &buf); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return withHeader("source: "+path, buf.Bytes()), nil
}

// ringExists reports whether any file of the rotation ring is on disk. Checking
// the rotated slots matters right after a rotation, when the live file can be
// empty while the history is very much there.
func ringExists(path string, maxFiles int) bool {
	for _, p := range append([]string{path}, logging.RotatedPaths(path, maxFiles)...) {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// withHeader stamps a one-line provenance comment on a collected text section.
// Whoever opens the bundle should not have to guess which file — or which
// machine's log source — they are looking at.
func withHeader(header string, body []byte) []byte {
	return append([]byte("# "+header+"\n\n"), body...)
}

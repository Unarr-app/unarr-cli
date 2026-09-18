package diagnostics

import (
	"bytes"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/logging"
)

const maxSourceBytes = 1024 * 1024
const maxSourceEvents = 1000

func readRegularTail(path string, budget int) ([]byte, string) {
	before, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, "absent"
	}
	if err != nil {
		return nil, "unreadable"
	}
	if !before.Mode().IsRegular() {
		return nil, "not-regular"
	}
	f, err := openDiagnosticFile(path)
	if err != nil {
		return nil, "unreadable"
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return nil, "unreadable"
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, "not-regular"
	}
	status := "ok"
	offset := int64(0)
	if after.Size() > int64(budget) {
		offset = after.Size() - int64(budget)
		status = "truncated"
	}
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return nil, "unreadable"
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(budget)))
	if err != nil {
		return nil, "unreadable"
	}
	return b, status
}

func collectLogFiles(source, path string) LogSource {
	s := LogSource{Source: source, Status: "absent", Events: []Event{}}
	// A fixed ring limit avoids unbounded directory enumeration and attacker
	// supplied rotation counts. Recent entries are preferred when budget fills.
	for slot := 0; slot <= 10; slot++ {
		name := path
		if slot > 0 {
			name += "." + strconv.Itoa(slot)
		}
		budget := maxSourceBytes - s.ScannedBytes
		if budget == 0 {
			s.Status = "truncated"
			break
		}
		b, status := readRegularTail(name, budget)
		if status == "absent" {
			continue
		}
		appendLogWindow(&s, b, status)
	}
	return s
}

func appendLogWindow(s *LogSource, b []byte, status string) {
	if s.Status == "absent" || status != "ok" {
		s.Status = status
	}
	s.ScannedBytes += len(b)
	// A window cut in the middle of a line cannot be classified reliably.
	if status == "truncated" {
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		} else {
			b = nil
		}
		s.OmittedLines++
	}
	scanEvents(s, b)
}

func collectJournal() LogSource {
	s := LogSource{Source: "journal", Status: "unsupported", Events: []Event{}}
	if runtime.GOOS != "linux" {
		return s
	}
	b, truncated, err := localCommand(maxSourceBytes, "journalctl", "--user", "-u", "unarr", "-n", "1000", "--no-pager", "--output=cat")
	if err != nil {
		s.Status = "unreadable"
		return s
	}
	s.Status = "ok"
	s.ScannedBytes = len(b)
	if truncated {
		s.Status = "truncated"
		if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
			b = b[:i+1]
		} else {
			b = nil
		}
	}
	scanEvents(&s, b)
	return s
}

// Every emitted byte of an event comes from a fixed literal, never a match
// capture. Unknown messages, stack frames and user-provided strings are omitted.
var eventMarkers = []struct{ marker, event string }{
	{"load failed: 5", "launchd_load_failed"},
	{"bootstrap failed:", "launchd_bootstrap_failed"},
	{"agent registered:", "daemon_registered"},
	{"daemon stopped", "daemon_stopped"},
	{"daemon started", "daemon_started"},
	{"shutting down", "daemon_shutdown"},
	{"already running", "already_running"},
	{"panic:", "panic"},
	{"fatal error:", "fatal_error"},
	{"out of memory", "out_of_memory"},
	{"permission denied", "permission_denied"},
	{"no space left on device", "disk_full"},
	{"connection refused", "connection_refused"},
	{"i/o timeout", "connection_timeout"},
	{"context deadline exceeded", "connection_timeout"},
	{"no such host", "dns_failed"},
	{"tls handshake", "tls_failed"},
	{"authentication failed", "authentication_failed"},
	{"invalid config", "config_invalid"},
	{"[vpn] tunnel failed", "vpn_failed"},
	{"[vpn] reconnect failed", "vpn_failed"},
	{"tunnel failed", "tunnel_failed"},
	{"task failed", "task_failed"},
	{"network is unreachable", "network_unreachable"},
	{"input/output error", "io_error"},
	{"startup failed", "startup_failed"},
}

func scanEvents(s *LogSource, b []byte) {
	for len(b) > 0 {
		line, rest, _ := bytes.Cut(b, []byte{'\n'})
		b = rest
		if len(line) == 0 {
			continue
		}
		if len(s.Events) >= maxSourceEvents {
			s.OmittedLines++
			s.Status = "truncated"
			continue
		}
		if event, matched := classifyEvent(string(line)); matched {
			s.Events = append(s.Events, event)
		} else {
			s.OmittedLines++
		}
	}
	s.OmittedLines = boundedInt(s.OmittedLines, maxSourceBytes)
}

func classifyEvent(line string) (Event, bool) {
	lower := strings.ToLower(line)
	for _, marker := range eventMarkers {
		if strings.Contains(lower, marker.marker) {
			return Event{Event: marker.event, Time: eventTime(line)}, true
		}
	}
	return Event{}, false
}

func eventTime(line string) string {
	timestamp := logging.ParseEntry(line).Time
	if timestamp.IsZero() {
		// ParseEntry's ISO layout uses an offset-sized prefix; accept the
		// shorter Z form too, without retaining the original text.
		prefix, _, _ := strings.Cut(line, " ")
		if len(prefix) <= 35 {
			timestamp, _ = time.Parse(time.RFC3339, prefix)
		}
	}
	if !timestamp.IsZero() && timestamp.Year() >= 1970 && timestamp.Year() <= 9999 {
		return timestamp.UTC().Truncate(time.Second).Format(time.RFC3339)
	}
	return ""
}

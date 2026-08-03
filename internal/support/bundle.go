// Package support builds the diagnostic archive `unarr support-bundle` writes.
//
// The command exists to collapse a support thread — "run doctor", "paste your
// config", "now the log", "and the daemon state" — into one attachable file.
// That only works if the user can attach it WITHOUT auditing it first, which is
// why this package is written around one rule:
//
//	Nothing reaches the bundle unless something here explicitly decided to put
//	it there.
//
// Concretely: the configuration is projected onto publishedConfig (an
// allowlist type, not a filtered copy — see redact_config.go), free text is run
// through the Scrubber, and every section is enumerated below. A field added to
// config.Config tomorrow is absent from the bundle by construction, and
// TestEveryConfigFieldIsClassified fails until someone says what it is.
//
// The bundle is written to a local file and is never uploaded, offered for
// upload, or transmitted anywhere. There is no network call in this package.
package support

import (
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"sort"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/doctor"
)

// DefaultLogLines is how much daemon log the bundle carries by default.
//
// 500 lines is the compromise between "enough to see the failure and what led
// to it" and "small enough to attach to a GitHub issue". Raise it with
// --log-lines when the interesting event is further back; the byte cap below
// still applies.
const DefaultLogLines = 500

// maxSectionBytes caps ANY single collected section.
//
// The line cap alone does not bound the size: logging.maxLineBytes lets one
// line reach 1 MiB (ffmpeg and the torrent client do produce those), so 500
// lines is worst-case half a gigabyte. 2 MiB per section keeps the archive in
// the range issue trackers accept while still holding thousands of ordinary
// lines. When a section is trimmed the TAIL is kept — the most recent output is
// the output that explains the failure — and the manifest records the trim.
// A var, not a const, so a test can shrink it: the behaviour under test is the
// trimming, not the size at which it happens, and building a 2 MiB fixture on
// every run costs fifteen seconds under -race for nothing.
var maxSectionBytes = 2 << 20

// Section is one file inside the bundle.
//
// A Section with a non-empty Absent carries no body and writes no file: the
// section is reported as missing, WITH the reason, instead of taking the whole
// command down. That is deliberate — a box with no daemon log is exactly the
// box whose bundle matters most, and "unarr.log: absent (no such file)" is
// itself a diagnosis.
type Section struct {
	Name   string `json:"name"`
	Bytes  int    `json:"bytes"`
	Absent string `json:"absent,omitempty"`
	Note   string `json:"note,omitempty"`

	body []byte
}

// Bundle is the collected, redacted content, before it is written anywhere.
// Collect never touches the filesystem for output; WriteTarGz does.
type Bundle struct {
	GeneratedAt time.Time `json:"generatedAt"`
	Version     string    `json:"version"`
	Platform    string    `json:"platform"`
	LogLines    int       `json:"logLines"`
	Sections    []Section `json:"sections"`
}

// Inputs is everything Collect needs that this package must not resolve for
// itself.
//
// Doctor and Journal are injected because their implementations live in
// internal/cmd (the doctor specs need its config/client helpers; the journal
// reader is `unarr logs`'s own). Injecting them is what makes the bundle embed
// the SAME report the user sees rather than a second, subtly different
// implementation of the same checks.
type Inputs struct {
	Config   config.Config
	Version  string
	LogLines int

	// Doctor runs the diagnostics and returns the report `unarr doctor --json`
	// would print. nil records the doctor section as absent.
	Doctor func() (doctor.Report, error)

	// Journal reads the last n lines of daemon output from the systemd journal.
	// Non-nil ONLY on a host where the daemon has no log file of its own —
	// under systemd there is no unarr.log to read, and a stale one left by an
	// earlier `unarr up` would be worse than nothing.
	Journal func(w io.Writer, n int) error

	// Logs names the daemon's log files. See LogPaths.
	Logs LogPaths

	// FFmpegPath is the resolved ffmpeg binary, or "" when there is none.
	FFmpegPath string

	// BenchCachePath is where `unarr bench` caches its encode measurement.
	// Passed in rather than imported so this package keeps no dependency on
	// internal/engine.
	BenchCachePath string
}

// lines resolves the requested history depth.
func (in Inputs) lines() int {
	if in.LogLines <= 0 {
		return DefaultLogLines
	}
	return in.LogLines
}

// collector is one section and the code that produces it. An error from fn is
// not a failure of the command: it becomes the section's Absent reason.
type collector struct {
	name string
	fn   func() ([]byte, error)
}

// Collect gathers and redacts everything. It performs no output I/O and makes
// no network call of its own — the only thing that can reach the network is the
// injected Doctor, which is the connectivity check the user asked for.
func Collect(in Inputs) *Bundle {
	scrub := NewScrubber(in.Config)
	b := &Bundle{
		GeneratedAt: time.Now().UTC(),
		Version:     in.Version,
		Platform:    runtime.GOOS + "/" + runtime.GOARCH,
		LogLines:    in.lines(),
	}
	for _, c := range collectors(in) {
		b.Sections = append(b.Sections, buildSection(c, scrub))
	}
	sort.SliceStable(b.Sections, func(i, j int) bool { return b.Sections[i].Name < b.Sections[j].Name })
	return b
}

// collectors is the exhaustive list of what a bundle contains. Adding a
// section means adding a line here; there is no discovery, no directory walk,
// nothing that could pick up a file we did not name.
func collectors(in Inputs) []collector {
	return []collector{
		{"version.txt", func() ([]byte, error) { return versionText(in), nil }},
		{"doctor.json", func() ([]byte, error) { return doctorJSON(in.Doctor) }},
		{"config.redacted.toml", func() ([]byte, error) { return configTOML(in.Config) }},
		{"unarr.log", func() ([]byte, error) { return daemonLogText(in) }},
		{"unarr.err.log", func() ([]byte, error) { return errLogText(in) }},
		{"unarr.boot.log", func() ([]byte, error) { return bootLogText(in) }},
		{"daemon.state.json", daemonStateJSON},
		{"tasks.json", activeTasksJSON},
		{"bench-encode.json", func() ([]byte, error) { return benchCache(in.BenchCachePath) }},
		{"system.txt", func() ([]byte, error) { return systemText(in.Config), nil }},
		{"network.txt", func() ([]byte, error) { return networkText(in.Config), nil }},
		{"ffmpeg.txt", func() ([]byte, error) { return ffmpegText(in) }},
	}
}

// buildSection runs one collector and applies the two rules every section
// obeys: scrub the bytes, then bound them.
func buildSection(c collector, scrub *Scrubber) Section {
	body, err := c.fn()
	if err != nil {
		return Section{Name: c.name, Absent: err.Error()}
	}
	body = scrub.Text(body)
	body, note := capBytes(body)
	return Section{Name: c.name, Bytes: len(body), Note: note, body: body}
}

// capBytes trims a section to maxSectionBytes, keeping the tail, and says so.
func capBytes(b []byte) ([]byte, string) {
	if len(b) <= maxSectionBytes {
		return b, ""
	}
	trimmed := len(b) - maxSectionBytes
	note := fmt.Sprintf("truncated: first %d bytes dropped (section capped at %d)", trimmed, maxSectionBytes)
	head := []byte("… " + note + " …\n")
	return append(head, b[trimmed:]...), note
}

// Manifest is the bundle's own index, written into the archive as
// manifest.json and printed by --print. Absent sections appear here WITH their
// reason, which is the only place that information survives.
func (b *Bundle) Manifest() ([]byte, error) {
	out, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

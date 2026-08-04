package support

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/doctor"
	"github.com/Unarr-app/unarr-cli/internal/winproc"
)

// ffmpegTimeout bounds `ffmpeg -version`. A support bundle must finish even
// when the configured binary is a stub on a dead network mount — the whole
// point is to produce something attachable, not to hang the way the thing being
// diagnosed hangs.
const ffmpegTimeout = 5 * time.Second

// versionText is the first thing anyone reading the bundle looks at: which
// build, on what.
func versionText(in Inputs) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "unarr:      %s\n", in.Version)
	fmt.Fprintf(&b, "platform:   %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "go:         %s\n", runtime.Version())
	fmt.Fprintf(&b, "generated:  %s\n", time.Now().UTC().Format(time.RFC3339))
	return b.Bytes()
}

// doctorJSON embeds the report `unarr doctor --json` produces, by running the
// same specs through the same runner. Re-implementing the checks here would
// give the user and the maintainer two different answers to the same question.
//
// Caveat worth knowing: unlike config.redacted.toml, this section is free text
// we did not shape — check messages quote paths and endpoints. It is scrubbed
// for credentials like every other text section, and it is the report the user
// already saw on their own terminal, but it is NOT allowlisted field by field.
func doctorJSON(run func() (doctor.Report, error)) ([]byte, error) {
	if run == nil {
		return nil, errors.New("absent: no doctor runner was wired in")
	}
	rep, err := run()
	if err != nil {
		return nil, fmt.Errorf("run doctor checks: %w", err)
	}
	var buf bytes.Buffer
	if err := doctor.RenderJSON(&buf, rep); err != nil {
		return nil, fmt.Errorf("render doctor report: %w", err)
	}
	return buf.Bytes(), nil
}

// systemText reports the host's shape and the free space on the directories
// unarr actually writes to. "Disk full" and "wrong PUID in Docker" are two of
// the three most common support threads; both are answered here.
func systemText(cfg config.Config) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "cpus:       %d\n", runtime.NumCPU())
	fmt.Fprintf(&b, "goroot os:  %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "uid/gid:    %s\n", processIdentity())
	fmt.Fprintf(&b, "in docker:  %t\n", inDocker())
	b.WriteString("\ndisk:\n")
	for _, d := range configuredDirs(cfg) {
		fmt.Fprintf(&b, "  %-12s %s\n", d.label+":", diskLine(d.path))
	}
	return b.Bytes()
}

// labelledDir pairs a config key with the path it resolves to. The label is
// published; the path is NOT — see diskLine.
type labelledDir struct {
	label string
	path  string
}

func configuredDirs(cfg config.Config) []labelledDir {
	return []labelledDir{
		{"downloads", cfg.Download.Dir},
		{"movies", cfg.Organize.MoviesDir},
		{"tv shows", cfg.Organize.TVShowsDir},
		{"hls cache", cfg.Download.HLSCache.Dir},
		{"backups", cfg.Library.BackupDir},
		{"data dir", config.DataDir()},
	}
}

// diskLine reports what a directory is DOING, not where it is. The path itself
// carries the account name on every platform ("/home/marta/…",
// "C:\Users\Marta\…"), and the numbers are what a support answer is built from.
func diskLine(path string) string {
	if strings.TrimSpace(path) == "" {
		return valueUnset
	}
	st, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return "configured, but DOES NOT EXIST"
	case err != nil:
		return "configured, but unreadable: " + err.Error()
	case !st.IsDir():
		return "configured, but is not a directory"
	}
	// Bounded, not the bare syscall: statfs(2) on a download dir that lives on a
	// dead SMB/NFS mount does not return, ever. A support bundle that hangs on
	// the very mount the user is reporting about is worse than useless.
	free, total, err := agent.DiskInfoBounded(path)
	if err != nil || total <= 0 {
		return "exists (free space unavailable)"
	}
	return fmt.Sprintf("exists — %s free of %s", humanBytes(free), humanBytes(total))
}

// networkText reports the ports the agent listens on and the state of the two
// things that most often explain "the web app cannot reach my agent": the
// Cloudflare funnel and the VPN kill-switch.
//
// Everything here comes from config and the daemon's own state file. No socket
// is opened and no external tool is run — a support bundle must not itself
// become the thing that hangs on a wedged network stack.
func networkText(cfg config.Config) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "peer listen port:   %d\n", cfg.Download.ListenPort)
	fmt.Fprintf(&b, "stream port (http): %d\n", cfg.Download.StreamPort)
	fmt.Fprintf(&b, "stream port (https):%d\n", cfg.Download.HTTPSStreamPort)
	fmt.Fprintf(&b, "upnp:               %t (auto https upnp: %t)\n", cfg.Download.EnableUPnP, cfg.Download.AutoHTTPSUpnp)
	fmt.Fprintf(&b, "webdav:             enabled=%t wan=%t\n", cfg.Download.WebDAVEnabled, cfg.Download.WebDAVAllowWAN)
	fmt.Fprintf(&b, "funnel configured:  %t\n", cfg.Download.Funnel.Enabled)
	fmt.Fprintf(&b, "vpn configured:     enabled=%t required=%t\n", cfg.Download.VPN.Enabled, cfg.Download.VPN.Required)

	st := agent.ReadState()
	if st == nil {
		b.WriteString("\nlive state:         absent (daemon not running)\n")
		return b.Bytes()
	}
	// The funnel URL is a capability: anyone holding that hostname can reach the
	// agent. Only whether one exists is published.
	fmt.Fprintf(&b, "\nfunnel live:        %t\n", st.FunnelURL != "")
	fmt.Fprintf(&b, "vpn live:           active=%t blocking=%t mode=%s\n", st.VPNActive, st.VPNBlocking, pick(st.VPNMode, "managed", "self-hosted"))
	fmt.Fprintf(&b, "vpn endpoint:       %s\n", presence(st.VPNServer))
	return b.Bytes()
}

// ffmpegText records which ffmpeg is in play and what it says about itself.
// Only the first lines of `ffmpeg -version` are kept: the configuration line
// lists every --enable flag of the build, which is hundreds of columns of noise
// that pushes the useful banner out of view.
func ffmpegText(in Inputs) ([]byte, error) {
	if in.FFmpegPath == "" {
		return nil, errors.New("absent: no ffmpeg binary found (transcoding and library scans need one)")
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "binary:     %s\n", presence(in.FFmpegPath))
	fmt.Fprintf(&b, "configured: ffmpeg_path=%s ffprobe_path=%s\n\n",
		presence(in.Config.Library.FFmpegPath), presence(in.Config.Library.FFprobePath))

	ctx, cancel := context.WithTimeout(context.Background(), ffmpegTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, in.FFmpegPath, "-version")
	winproc.HideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		fmt.Fprintf(&b, "ffmpeg -version failed: %v\n", err)
		return b.Bytes(), nil
	}
	b.Write(firstLines(out, 3))
	return b.Bytes(), nil
}

// benchCache embeds `unarr bench`'s cached encode measurement verbatim. It is
// machine-generated JSON about this host's hardware — no user input reaches it
// — and it is the only record of how fast this box actually encodes, which is
// the answer to half the "streaming stutters" reports.
func benchCache(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("absent: no bench cache path was wired in")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("absent: no cached benchmark (run `unarr bench`): %w", err)
	}
	return data, nil
}

// firstLines returns at most n lines of b.
func firstLines(b []byte, n int) []byte {
	lines := bytes.SplitAfter(b, []byte("\n"))
	if len(lines) > n {
		lines = lines[:n]
	}
	return bytes.Join(lines, nil)
}

// processIdentity reports the effective uid/gid. It is the first thing to check
// on a NAS: a PUID/PGID mismatch makes every write fail with a permission error
// that looks like a bug in unarr.
func processIdentity() string {
	if runtime.GOOS == "windows" {
		return "n/a on windows"
	}
	return strconv.Itoa(os.Getuid()) + "/" + strconv.Itoa(os.Getgid())
}

// inDocker reports whether we are running inside the official image. The
// marker file is written by the Dockerfile; /.dockerenv covers the generic case.
func inDocker() bool {
	for _, p := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return os.Getenv("UNARR_IN_DOCKER") != ""
}

// humanBytes renders a byte count in the unit a human reads at a glance.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

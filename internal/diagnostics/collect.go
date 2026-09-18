package diagnostics

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/sysinfo"
	"github.com/google/uuid"
)

var numericVersion = regexp.MustCompile(`^[0-9]{1,4}(?:\.[0-9]{1,4}){0,3}$`)
var releaseVersion = regexp.MustCompile(`^[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,4}(?:-(?:alpha|beta|rc)(?:[.-][0-9]{1,4})?)?$`)

// Collect reads bounded local diagnostic sources. It performs no HTTP requests,
// registration, telemetry, doctor checks or configuration mutations.
func Collect(cfg *config.Config, version string) (Report, error) {
	if cfg == nil {
		return Report{}, errors.New("diagnostic report requires agent configuration")
	}
	id, err := uuid.Parse(cfg.Agent.ID)
	if err != nil || id == uuid.Nil || id.String() != cfg.Agent.ID {
		return Report{}, errors.New("diagnostic report requires a canonical agent UUID")
	}
	r := Report{SchemaVersion: 1, AgentID: id.String(), GeneratedAt: time.Now().UTC().Format(time.RFC3339)}
	r.System = System{OS: enum(runtime.GOOS, "linux", "darwin", "windows", "freebsd"), Arch: enum(runtime.GOARCH, "amd64", "arm64", "386", "arm"), OSVersion: operatingSystemVersion(), AppVersion: safeVersion(strings.TrimPrefix(version, "v"), releaseVersion), GoVersion: safeVersion(strings.TrimPrefix(runtime.Version(), "go"), numericVersion), CPUs: boundedInt(runtime.NumCPU(), 4096)}
	if n, ok := sysinfo.TotalMemory(); ok {
		r.System.MemoryMiB = boundedMiB(n)
	}
	if cfg.Download.Dir != "" {
		if free, total, err := agent.DiskInfoBounded(cfg.Download.Dir); err == nil && free >= 0 && total >= 0 {
			r.System.DownloadFreeMiB, r.System.DownloadTotalMiB = boundedMiB(uint64(free)), boundedMiB(uint64(total))
		}
	}
	r.Settings = Settings{VPNEnabled: cfg.Download.VPN.Enabled, VPNRequired: cfg.Download.VPN.Required, FunnelEnabled: cfg.Download.Funnel.Enabled, WebDAVEnabled: cfg.Download.WebDAVEnabled, AutoUpgrade: cfg.Daemon.AutoUpgrade == nil || *cfg.Daemon.AutoUpgrade}
	r.Service = collectService()
	collectDaemonState(&r.Service, agent.StateFilePath())
	for _, src := range []struct{ name, file string }{{"daemon", "unarr.log"}, {"boot", "unarr.boot.log"}, {"legacy-error", "unarr.err.log"}} {
		r.Logs = append(r.Logs, collectLogFiles(src.name, filepath.Join(config.DataDir(), src.file)))
	}
	// The desktop currently retains its own output in memory, not in a stable
	// file. Do not invent a path or inspect another process's memory.
	r.Logs = append(r.Logs, LogSource{Source: "desktop", Status: "unsupported", Events: []Event{}})
	r.Logs = append(r.Logs, collectJournal())
	remaining := maxSourceEvents
	for i := range r.Logs {
		if len(r.Logs[i].Events) > remaining {
			r.Logs[i].OmittedLines = boundedInt(r.Logs[i].OmittedLines+len(r.Logs[i].Events)-remaining, maxSourceBytes)
			r.Logs[i].Events = r.Logs[i].Events[:remaining]
			r.Logs[i].Status = "truncated"
		}
		remaining -= len(r.Logs[i].Events)
	}
	return r, nil
}

func enum(value string, allowed ...string) string {
	for _, a := range allowed {
		if value == a {
			return a
		}
	}
	return "unknown"
}

func safeVersion(value string, pattern *regexp.Regexp) string {
	if len(value) <= 32 && pattern.MatchString(value) {
		return value
	}
	return "unknown"
}

func boundedInt(n, max int) int {
	if n < 0 {
		return 0
	}
	if n > max {
		return max
	}
	return n
}
func boundedMiB(n uint64) uint64 {
	n /= 1024 * 1024
	if n > 4294967295 {
		return 4294967295
	}
	return n
}

func collectDaemonState(s *Service, path string) {
	b, status := readRegularTail(path, 64*1024)
	if status != "ok" {
		return
	}
	var state struct {
		Status      string `json:"status"`
		ActiveTasks int    `json:"activeTasks"`
		PID         int    `json:"pid"`
	}
	if json.Unmarshal(b, &state) != nil {
		return
	}
	s.DaemonState = enum(state.Status, "running", "stopped", "blocked")
	if s.DaemonState == "running" && (state.PID <= 0 || !agent.IsProcessAlive(state.PID)) {
		s.DaemonState = "stopped"
	}
	s.ActiveTasks = boundedInt(state.ActiveTasks, 1000000)
}

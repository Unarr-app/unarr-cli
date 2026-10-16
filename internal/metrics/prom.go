package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// ContentType is the Prometheus text exposition format, version 0.0.4.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// WriteProm renders a snapshot in the Prometheus text exposition format.
//
// Written by hand rather than through client_golang: the format is a few dozen
// lines of text and the daemon exposes ~10 fixed series, while client_golang
// (+ client_model, common, procfs, protobuf) is several MB of binary and a long
// dependency chain for a feature most users leave off. The cost of that choice
// is that we own the escaping and the HELP/TYPE ordering; prom_test.go parses
// the output back to hold us to it.
//
// Labels are limited to kind, state and version — never an id, hash or title —
// so cardinality is bounded by the Kind set and the download state vocabulary.
func WriteProm(w io.Writer, s Snapshot) error {
	p := &promWriter{w: w}
	p.header("unarr_info", "gauge", "Agent build information (value is always 1).")
	p.sample("unarr_info", 1, "version", s.Agent.Version)
	p.header("unarr_uptime_seconds", "gauge", "Seconds since the daemon started.")
	p.sample("unarr_uptime_seconds", s.Agent.UptimeSec)

	p.header("unarr_sessions_active", "gauge", "Active sessions by kind.")
	for _, k := range Kinds {
		p.sample("unarr_sessions_active", s.Summary.Active[k], "kind", string(k))
	}
	p.header("unarr_sessions_stalled", "gauge", "Sessions that are stalled right now.")
	p.sample("unarr_sessions_stalled", s.Summary.Stalled)

	p.header("unarr_session_bytes_total", "counter", "Bytes moved by sessions since daemon start, by kind.")
	for _, k := range Kinds {
		p.sample("unarr_session_bytes_total", s.Summary.BytesTotal[k], "kind", string(k))
	}
	p.header("unarr_session_rate_bytes_per_second", "gauge", "Current aggregate rate of active sessions, by kind.")
	for _, k := range Kinds {
		p.sample("unarr_session_rate_bytes_per_second", s.Summary.RateBps[k], "kind", string(k))
	}
	p.header("unarr_session_stalls_total", "counter", "Stall episodes since daemon start, by kind (one per stall, not per tick).")
	for _, k := range Kinds {
		p.sample("unarr_session_stalls_total", s.Summary.StallsTotal[k], "kind", string(k))
	}

	p.header("unarr_vpn_up", "gauge", "1 when the managed VPN tunnel is healthy.")
	p.sample("unarr_vpn_up", b2i(s.Summary.VPN.Up))
	p.header("unarr_vpn_enabled", "gauge", "1 when the managed VPN is enabled in config.")
	p.sample("unarr_vpn_enabled", b2i(s.Summary.VPN.Enabled))
	p.header("unarr_vpn_required", "gauge", "1 when the VPN kill-switch (fail-closed P2P) is on.")
	p.sample("unarr_vpn_required", b2i(s.Summary.VPN.Required))

	p.header("unarr_downloads", "gauge", "Known downloads by state.")
	states := make([]string, 0, len(s.Summary.DownloadsByState))
	for st := range s.Summary.DownloadsByState {
		states = append(states, st)
	}
	sort.Strings(states)
	for _, st := range states {
		p.sample("unarr_downloads", s.Summary.DownloadsByState[st], "state", st)
	}
	return p.err
}

type promWriter struct {
	w   io.Writer
	err error
}

func (p *promWriter) printf(format string, a ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintf(p.w, format, a...)
	}
}

func (p *promWriter) header(name, typ, help string) {
	p.printf("# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

// sample writes one series; labels are name/value pairs.
func (p *promWriter) sample(name string, v any, labels ...string) {
	var sb strings.Builder
	for i := 0; i+1 < len(labels); i += 2 {
		if sb.Len() > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `%s="%s"`, labels[i], escapeLabel(labels[i+1]))
	}
	if sb.Len() > 0 {
		p.printf("%s{%s} %v\n", name, sb.String(), v)
		return
	}
	p.printf("%s %v\n", name, v)
}

// escapeLabel escapes a label value per the exposition format.
func escapeLabel(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

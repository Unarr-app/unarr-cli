package metrics

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// promSample is one parsed series line.
type promSample struct {
	name   string
	labels map[string]string
	value  float64
}

var (
	promName  = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	promLabel = regexp.MustCompile(`^([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"$`)
)

// parseProm is a strict parser for the text exposition format: every sample
// must be preceded by its HELP and TYPE, names/labels must be well formed and
// values must be numbers. It fails the test on anything a real scraper would
// reject.
func parseProm(t *testing.T, body string) []promSample {
	t.Helper()
	var out []promSample
	help, typ := map[string]bool{}, map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "# HELP "):
			help[strings.Fields(line)[2]] = true
		case strings.HasPrefix(line, "# TYPE "):
			f := strings.Fields(line)
			if f[3] != "gauge" && f[3] != "counter" {
				t.Fatalf("bad type in %q", line)
			}
			typ[f[2]] = f[3]
		case strings.HasPrefix(line, "#"):
			t.Fatalf("unexpected comment %q", line)
		default:
			out = append(out, parsePromSample(t, line, help, typ))
		}
	}
	return out
}

func parsePromSample(t *testing.T, line string, help map[string]bool, typ map[string]string) promSample {
	t.Helper()
	nameAndLabels, val, ok := strings.Cut(line, " ")
	if !ok {
		t.Fatalf("no value in %q", line)
	}
	v, err := strconv.ParseFloat(val, 64)
	if err != nil {
		t.Fatalf("bad value in %q: %v", line, err)
	}
	s := promSample{labels: map[string]string{}, value: v}
	name, rest, hasLabels := strings.Cut(nameAndLabels, "{")
	s.name = name
	if !promName.MatchString(name) {
		t.Fatalf("bad metric name %q", name)
	}
	if !help[name] || typ[name] == "" {
		t.Fatalf("%s sampled before HELP/TYPE", name)
	}
	if typ[name] == "counter" && !strings.HasSuffix(name, "_total") {
		t.Fatalf("counter %s must end in _total", name)
	}
	if hasLabels {
		inner, ok := strings.CutSuffix(rest, "}")
		if !ok {
			t.Fatalf("unterminated labels in %q", line)
		}
		for _, pair := range splitLabels(inner) {
			m := promLabel.FindStringSubmatch(pair)
			if m == nil {
				t.Fatalf("bad label %q in %q", pair, line)
			}
			s.labels[m[1]] = m[2]
		}
	}
	return s
}

// splitLabels splits on commas outside quotes.
func splitLabels(s string) []string {
	var parts []string
	inQ, esc, start := false, false, 0
	for i, r := range s {
		switch {
		case esc:
			esc = false
		case r == '\\':
			esc = true
		case r == '"':
			inQ = !inQ
		case r == ',' && !inQ:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

func find(samples []promSample, name string, kv ...string) (promSample, bool) {
next:
	for _, s := range samples {
		if s.name != name {
			continue
		}
		for i := 0; i+1 < len(kv); i += 2 {
			if s.labels[kv[i]] != kv[i+1] {
				continue next
			}
		}
		return s, true
	}
	return promSample{}, false
}

func sampleSnapshot() Snapshot {
	return Snapshot{
		SchemaVersion: SchemaVersion,
		Agent:         Agent{Version: `1.2.3-"beta"`, UptimeSec: 42},
		Summary: Summary{
			Active:           map[Kind]int{KindDownload: 2, KindHLS: 1},
			Stalled:          1,
			BytesTotal:       map[Kind]int64{KindDownload: 1 << 30, KindMount: 7},
			StallsTotal:      map[Kind]int64{KindHLS: 3},
			RateBps:          map[Kind]int64{KindDownload: 5000},
			DownloadsByState: map[string]int{"downloading": 2, "paused": 1},
			VPN:              VPN{Enabled: true, Required: true, Up: false},
		},
	}
}

func TestWritePromIsParseable(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteProm(&buf, sampleSnapshot()); err != nil {
		t.Fatal(err)
	}
	samples := parseProm(t, buf.String())

	want := []struct {
		name string
		kv   []string
		val  float64
	}{
		{"unarr_uptime_seconds", nil, 42},
		{"unarr_sessions_active", []string{"kind", "download"}, 2},
		{"unarr_sessions_active", []string{"kind", "mount"}, 0}, // every kind always present
		{"unarr_sessions_stalled", nil, 1},
		{"unarr_session_bytes_total", []string{"kind", "download"}, 1 << 30},
		{"unarr_session_rate_bytes_per_second", []string{"kind", "download"}, 5000},
		{"unarr_session_stalls_total", []string{"kind", "hls"}, 3},
		{"unarr_vpn_up", nil, 0},
		{"unarr_vpn_enabled", nil, 1},
		{"unarr_vpn_required", nil, 1},
		{"unarr_downloads", []string{"state", "paused"}, 1},
		{"unarr_downloads", []string{"state", "downloading"}, 2},
	}
	for _, w := range want {
		got, ok := find(samples, w.name, w.kv...)
		if !ok || got.value != w.val {
			t.Errorf("%s%v = %v (found=%v), want %v", w.name, w.kv, got.value, ok, w.val)
		}
	}
	info, ok := find(samples, "unarr_info")
	if !ok || info.value != 1 || info.labels["version"] != `1.2.3-\"beta\"` {
		t.Errorf("unarr_info = %+v (label must be escaped)", info)
	}
}

// Cardinality guard: the only label names ever emitted are kind, state and
// version — never an id, hash or title.
func TestWritePromLabelNamesAreBounded(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteProm(&buf, sampleSnapshot())
	allowed := map[string]bool{"kind": true, "state": true, "version": true}
	for _, s := range parseProm(t, buf.String()) {
		for l := range s.labels {
			if !allowed[l] {
				t.Errorf("%s has label %q", s.name, l)
			}
		}
	}
}

func TestEscapeLabel(t *testing.T) {
	got := escapeLabel("a\\b\"c\nd")
	if want := `a\\b\"c\nd`; got != want {
		t.Fatalf("escapeLabel = %q, want %q", got, want)
	}
	// And the whole line still parses.
	var buf bytes.Buffer
	snap := sampleSnapshot()
	snap.Agent.Version = "x\ny"
	_ = WriteProm(&buf, snap)
	parseProm(t, buf.String())
	if strings.Count(buf.String(), "\n") < 10 {
		t.Fatal(fmt.Sprint("suspiciously short output: ", buf.String()))
	}
}

package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// readmeAssignRe matches a live TOML assignment ("max_concurrent = 3").
var readmeAssignRe = regexp.MustCompile(`^([A-Za-z0-9_]+)\s*=`)

// readmeCommentedRe matches a key the README shows commented out
// (`# webdav_username = "unarr"`) — an invented key hurts exactly as much
// there, since the reader's next move is to uncomment it. Narrower than the
// live form on purpose: the value must LOOK like TOML, so ordinary prose in a
// comment ("# Empty = autodetect") is not mistaken for a key.
var readmeCommentedRe = regexp.MustCompile(`^#\s*([a-z0-9_]+)\s*=\s*(["'\[]|true\b|false\b|-?\d)`)

// readmeTableRe matches a table header ("[library.cleanup]").
var readmeTableRe = regexp.MustCompile(`^\[([A-Za-z0-9_.]+)\]$`)

// readmeKey is one key the README documents, with the source line so a failure
// points at the text to fix.
type readmeKey struct {
	line int
	name string
	// bare marks a key from a snippet with no [table] header (the README shows
	// a few of those as fragments). Those can only be checked by leaf name.
	bare bool
}

// TestREADMETOMLKeysExist checks every key in every ```toml block of the README
// against the schema.
//
// The README is the file users copy from, so a key Config does not have is not
// a harmless doc typo: it is `unarr config check` exiting 1, a permanent doctor
// WARN and a "[config] unknown key" line on every daemon start — handed to the
// user by the project itself. ("poll_interval"/"heartbeat_interval" sat in the
// documented [daemon] block for exactly that long.)
func TestREADMETOMLKeysExist(t *testing.T) {
	valid := make(map[string]bool)
	leaf := make(map[string]bool)
	for _, k := range ValidKeys() {
		valid[k] = true
		leaf[k[strings.LastIndex(k, ".")+1:]] = true
	}

	keys := readmeTOMLKeys(t)
	if len(keys) < 20 {
		t.Fatalf("parsed only %d keys out of the README's toml blocks — the parser, not the doc, is broken", len(keys))
	}
	for _, k := range keys {
		if k.bare && leaf[k.name] {
			continue
		}
		if valid[k.name] {
			continue
		}
		t.Errorf("README.md:%d documents %q, which config.Config has no field for%s",
			k.line, k.name, suggestion(k.name))
	}
}

// suggestion appends the nearest real key, reusing the same matcher `unarr
// config check` shows the user.
func suggestion(key string) string {
	if s := SuggestKey(key, ValidKeys()); s != "" {
		return " — did you mean " + s + "?"
	}
	return ""
}

// readmeTOMLKeys extracts the table headers and assignments of every ```toml
// fenced block in the README.
func readmeTOMLKeys(t *testing.T) []readmeKey {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}

	var out []readmeKey
	var table string
	inBlock := false
	for i, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "```") {
			inBlock = !inBlock && strings.HasPrefix(line, "```toml")
			table = ""
			continue
		}
		if !inBlock {
			continue
		}
		out = append(out, readmeLineKeys(strings.TrimSpace(line), i+1, &table)...)
	}
	return out
}

// readmeLineKeys turns one line of a toml block into the keys it documents,
// updating the current table as it goes.
func readmeLineKeys(line string, num int, table *string) []readmeKey {
	if m := readmeTableRe.FindStringSubmatch(line); m != nil {
		*table = m[1]
		return []readmeKey{{line: num, name: m[1]}}
	}
	m := readmeAssignRe.FindStringSubmatch(line)
	if m == nil {
		m = readmeCommentedRe.FindStringSubmatch(line)
	}
	if m == nil {
		return nil
	}
	if *table == "" {
		return []readmeKey{{line: num, name: m[1], bare: true}}
	}
	return []readmeKey{{line: num, name: *table + "." + m[1]}}
}

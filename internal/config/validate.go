package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// Issue is one problem found in a config file: a key the schema does not know,
// or a value outside its accepted range. Message is the line shown to the user;
// Key and Suggestion stay separate so callers (and tests) can inspect a finding
// without parsing prose.
type Issue struct {
	Key        string // dotted TOML key, e.g. "downloads.min_free_disk_gb"
	Message    string // user-facing one-liner
	Suggestion string // nearest valid key, empty when nothing was close enough
}

func (i Issue) String() string { return i.Message }

// UnknownKeys returns the dotted TOML keys the last Load could not map onto the
// schema — a typo, or a valid key written under the wrong section. Nil for a
// Config built by Default() or decoded from a clean file. The backing field is
// unexported so the TOML encoder skips it: Save must never serialise
// diagnostics back into the user's file.
func (c *Config) UnknownKeys() []string { return c.unknownKeys }

// UnknownKeyIssues pairs every unrecognised key with the closest valid key, so
// the warning tells the user what to write instead of only what is wrong.
func (c *Config) UnknownKeyIssues() []Issue {
	return unknownKeyIssues(c.unknownKeys, ValidKeys())
}

// unknownKeyIssues is the pure core of UnknownKeyIssues, taking the valid-key
// set explicitly so it can be exercised without building a whole Config.
func unknownKeyIssues(unknown, valid []string) []Issue {
	out := make([]Issue, 0, len(unknown))
	for _, key := range topLevelUnknown(unknown) {
		iss := Issue{Key: key, Suggestion: SuggestKey(key, valid)}
		if iss.Suggestion != "" {
			iss.Message = fmt.Sprintf("unknown key %q - did you mean %q?", key, iss.Suggestion)
		} else {
			iss.Message = fmt.Sprintf("unknown key %q", key)
		}
		out = append(out, iss)
	}
	return out
}

// topLevelUnknown drops keys nested under another unknown key. BurntSushi/toml
// reports an unrecognised table AND every leaf inside it ("nosuchsection",
// "nosuchsection.a", "nosuchsection.b.c", …); naming the table once is the
// whole signal, and the per-leaf noise would bury the real typos.
func topLevelUnknown(keys []string) []string {
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted) // a parent always sorts before its children
	out := make([]string, 0, len(sorted))
	for _, k := range sorted {
		if n := len(out); n > 0 && strings.HasPrefix(k, out[n-1]+".") {
			continue
		}
		out = append(out, k)
	}
	return out
}

// ValidKeys returns every dotted TOML key Config accepts, derived from the
// struct tags themselves so the set can never drift from the schema. Both leaf
// keys and the tables holding them are listed ("downloads", "downloads.dir").
func ValidKeys() []string {
	return structKeys(reflect.TypeOf(Config{}), "")
}

// structKeys walks a struct type and returns the dotted TOML keys under prefix.
func structKeys(t reflect.Type, prefix string) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // unexported: invisible to the TOML codec too
		}
		name := tomlKeyName(f)
		if name == "" {
			continue
		}
		if prefix != "" {
			name = prefix + "." + name
		}
		out = append(out, name)
		if ft := derefType(f.Type); ft.Kind() == reflect.Struct {
			out = append(out, structKeys(ft, name)...)
		}
	}
	return out
}

// tomlKeyName resolves the TOML name of a struct field: the tag minus its
// options ("desktop,omitempty" → "desktop"), or the field name when untagged,
// mirroring what BurntSushi/toml does. "-" means the field is never encoded.
func tomlKeyName(f reflect.StructField) string {
	tag, _, _ := strings.Cut(f.Tag.Get("toml"), ",")
	switch tag {
	case "-":
		return ""
	case "":
		return f.Name
	}
	return tag
}

// derefType unwraps pointer types so a *bool / *SubConfig field is classified
// by what it points at.
func derefType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// SuggestKey returns the valid key closest to unknown, or "" when nothing is
// near enough that printing it would help more than it confuses.
//
// Two passes, in order of confidence: a key written under the WRONG SECTION
// ("general.max_concurrent") is matched on its leaf name first, because the
// section prefix alone blows any sane edit-distance budget; only then does a
// plain typo ("min_free_disk_gb") get matched on the whole dotted key.
func SuggestKey(unknown string, valid []string) string {
	if contains(valid, unknown) {
		return "" // not actually unknown — nothing to suggest
	}
	leaf := lastSegment(unknown)
	parent := parentPath(unknown)

	best, bestDist := "", 0
	for _, cand := range valid {
		if lastSegment(cand) != leaf {
			continue
		}
		if d := levenshtein(parent, parentPath(cand)); best == "" || d < bestDist {
			best, bestDist = cand, d
		}
	}
	if best != "" {
		return best
	}

	budget := suggestionBudget(leaf)
	for _, cand := range valid {
		if d := levenshtein(unknown, cand); d <= budget && (best == "" || d < bestDist) {
			best, bestDist = cand, d
		}
	}
	return best
}

// suggestionBudget is the largest edit distance still worth printing. It scales
// with the leaf name so a 3-letter key like "dir" doesn't attract every other
// short key, while a long "min_free_disk_mb" still tolerates a mistyped word.
func suggestionBudget(leaf string) int {
	switch {
	case len(leaf) >= 8:
		return 3
	case len(leaf) >= 5:
		return 2
	default:
		return 1
	}
}

// lastSegment returns the leaf name of a dotted key ("a.b.c" → "c").
func lastSegment(key string) string {
	if i := strings.LastIndex(key, "."); i >= 0 {
		return key[i+1:]
	}
	return key
}

// parentPath returns the table part of a dotted key ("a.b.c" → "a.b"), empty
// for a top-level key.
func parentPath(key string) string {
	if i := strings.LastIndex(key, "."); i >= 0 {
		return key[:i]
	}
	return ""
}

// levenshtein is the classic edit distance (insert / delete / substitute),
// computed over runes with a single rolling row.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"reflect"
	"strings"

	"github.com/BurntSushi/toml"
)

// renderConfig returns the bytes Save writes to path. With no file yet it is the
// plain encoding of cfg; otherwise the existing file is patched in place so the
// user's comments, key order and keys this build does not know (a newer CLI's,
// or a typo) survive, and only values that actually differ are rewritten.
func renderConfig(cfg Config, path string) ([]byte, error) {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	orig, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return buf.Bytes(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return mergeConfigText(orig, buf.Bytes())
}

// mergeConfigText applies encoded's values onto orig. Every candidate is
// re-decoded and must hold exactly cfg's values plus orig's unknown keys before
// it is returned; if neither the in-place patch nor the re-encode fallback
// passes, it errors and Save leaves the file untouched.
func mergeConfigText(orig, encoded []byte) ([]byte, error) {
	if len(bytes.TrimSpace(orig)) == 0 {
		return encoded, nil
	}
	oldTree, unknown, err := decodeWithUnknown(string(orig))
	if err != nil {
		// Load refuses this file too; keep the historical overwrite so a repair
		// path (init, doctor --fix) can still replace it.
		return encoded, nil
	}
	newTree := map[string]any{}
	if _, err := toml.Decode(string(encoded), &newTree); err != nil {
		return nil, fmt.Errorf("decode encoded config: %w", err)
	}
	oldFlat, newFlat := flatten(oldTree), flatten(newTree)
	want := maps.Clone(newFlat)
	for k := range unknown {
		if v, ok := oldFlat[k]; ok {
			want[k] = v
		}
	}
	if out, ok := patchText(string(orig), string(encoded), oldFlat, newFlat, unknown); ok && holdsValues(out, want) {
		return []byte(out), nil
	}
	if out, ok := encodeWithUnknown(encoded, newTree, oldFlat, unknown); ok && holdsValues(string(out), want) {
		return out, nil
	}
	return nil, errors.New("merge config: could not update the existing file safely; it was left untouched")
}

// decodeWithUnknown decodes s generically and reports which leaf keys the
// Config schema does not know.
func decodeWithUnknown(s string) (map[string]any, map[string]bool, error) {
	tree := map[string]any{}
	if _, err := toml.Decode(s, &tree); err != nil {
		return nil, nil, err
	}
	var c Config
	meta, err := toml.Decode(s, &c)
	if err != nil {
		return nil, nil, err
	}
	unknown := map[string]bool{}
	for _, k := range meta.Undecoded() {
		unknown[pathKey(k)] = true
	}
	return tree, unknown, nil
}

// flatten maps every leaf value of tree by its key path.
func flatten(tree map[string]any) map[string]any {
	out := map[string]any{}
	flattenInto(out, nil, tree)
	return out
}

func flattenInto(out map[string]any, prefix []string, m map[string]any) {
	for k, v := range m {
		p := append(append([]string{}, prefix...), k)
		if sub, ok := v.(map[string]any); ok {
			flattenInto(out, p, sub)
			continue
		}
		out[pathKey(p)] = v
	}
}

// holdsValues reports whether text decodes to exactly the leaves in want.
func holdsValues(text string, want map[string]any) bool {
	tree := map[string]any{}
	if _, err := toml.Decode(text, &tree); err != nil {
		return false
	}
	got := flatten(tree)
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || !reflect.DeepEqual(g, v) {
			return false
		}
	}
	return true
}

// encodeWithUnknown is the fallback: the plain encoding (the pre-merge Save
// output) with orig's unknown keys grafted back. Comments and order are lost,
// data is not.
func encodeWithUnknown(encoded []byte, tree, oldFlat map[string]any, unknown map[string]bool) ([]byte, bool) {
	grafted := false
	for k := range unknown {
		v, ok := oldFlat[k]
		if !ok {
			continue
		}
		if !setPath(tree, strings.Split(k, keySep), v) {
			return nil, false
		}
		grafted = true
	}
	if !grafted {
		return encoded, true
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(tree); err != nil {
		return nil, false
	}
	return buf.Bytes(), true
}

func setPath(tree map[string]any, p []string, v any) bool {
	for _, k := range p[:len(p)-1] {
		next, exists := tree[k]
		if !exists {
			next = map[string]any{}
			tree[k] = next
		}
		sub, ok := next.(map[string]any)
		if !ok {
			return false
		}
		tree = sub
	}
	tree[p[len(p)-1]] = v
	return true
}

// patch collects line edits against the original document.
type patch struct {
	od, nd    *tomlDoc
	cr        string           // "\r" when the file uses CRLF line endings
	edits     map[int]lineEdit // first line → replacement
	after     map[int][]string // line → lines inserted after it (-1 = top)
	tail      []string         // new tables appended at the end
	newTables map[string]bool
}

type lineEdit struct {
	last int
	text []string
}

func patchText(orig, encoded string, oldFlat, newFlat map[string]any, unknown map[string]bool) (string, bool) {
	od, ok := scanDoc(orig)
	if !ok {
		return "", false
	}
	nd, ok := scanDoc(encoded)
	if !ok {
		return "", false
	}
	p := &patch{od: od, nd: nd, edits: map[int]lineEdit{}, after: map[int][]string{}, newTables: map[string]bool{}}
	if strings.Contains(orig, "\r\n") {
		p.cr = "\r"
	}
	for _, k := range nd.order {
		if ov, ok := oldFlat[k]; ok && reflect.DeepEqual(ov, newFlat[k]) {
			continue
		}
		if !p.set(k) {
			return "", false
		}
	}
	for k := range oldFlat {
		if _, ok := newFlat[k]; ok || unknown[k] {
			continue
		}
		// A known key cfg no longer carries (an omitempty field cleared): drop it,
		// or Load would bring the old value back.
		e, ok := od.entries[k]
		if !ok {
			return "", false
		}
		p.edits[e.first] = lineEdit{last: e.last}
	}
	return p.render(), true
}

// set writes encoded key k into the original: in place when it exists, else
// appended to its table, else in a new table at the end of the file.
func (p *patch) set(k string) bool {
	ne := p.nd.entries[k]
	if e, ok := p.od.entries[k]; ok {
		line := p.od.lines[e.first][:e.valStart] + p.nd.valueText(ne) + p.od.lines[e.last][e.valEnd:]
		p.edits[e.first] = lineEdit{last: e.last, text: []string{line}}
		return true
	}
	path := p.nd.paths[k]
	tk := pathKey(path[:len(path)-1])
	text := strings.TrimLeft(p.nd.lines[ne.first], " \t")
	if ne.last != ne.first {
		return false // the encoder writes every value on one line
	}
	if at, ok := p.od.tables[tk]; ok {
		p.after[at] = append(p.after[at], p.indentAt(at)+text+p.cr)
		return true
	}
	if !p.newTables[tk] {
		h, ok := p.nd.headers[tk]
		_, inline := p.od.entries[tk]
		// BurntSushi tolerates a [header] for a table already defined by dotted
		// keys or an inline table, but TOML forbids it: re-encode instead.
		if !ok || inline || p.od.dotted[tk] {
			return false
		}
		p.newTables[tk] = true
		p.tail = append(p.tail, p.cr, strings.TrimLeft(p.nd.lines[h], " \t")+p.cr)
	}
	p.tail = append(p.tail, text+p.cr)
	return true
}

func (p *patch) indentAt(i int) string {
	if i < 0 {
		return ""
	}
	line := p.od.lines[i]
	return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
}

func (p *patch) render() string {
	lines := p.od.lines
	hasNL := lines[len(lines)-1] == ""
	if hasNL {
		lines = lines[:len(lines)-1]
	}
	out := append([]string{}, p.after[-1]...)
	for i := 0; i < len(lines); i++ {
		end := i
		if e, ok := p.edits[i]; ok {
			out = append(out, e.text...)
			end = e.last
		} else {
			out = append(out, lines[i])
		}
		out = append(out, p.after[end]...)
		i = end
	}
	out = append(out, p.tail...)
	if hasNL || len(p.tail) > 0 {
		out = append(out, "")
	}
	return strings.Join(out, "\n")
}

package support

import (
	"reflect"
	"sort"
	"strings"
)

// Sensitivity is the publishing verdict for one leaf field of config.Config.
//
// Every field gets one, and the verdict is recorded in configFields below —
// not inferred from the field's name. A name-based rule is a denylist wearing
// a costume: it works until someone adds `WebdavPasswd` or `Cookie` and the
// pattern misses it, and the miss ships the user's credential to a public
// GitHub issue. An exhaustive map cannot miss; it can only be *incomplete*,
// and TestEveryConfigFieldIsClassified turns incompleteness into a red build.
type Sensitivity int

const (
	// Publishable means the field's SHAPE may appear in the bundle — see
	// redact_config.go, which decides how. It is not a licence to print the
	// value verbatim: a Publishable free-form string is still published as
	// "set"/"unset" or as a vocabulary match, never as the raw bytes.
	Publishable Sensitivity = iota
	// Secret means the value is a credential. It never appears in the bundle,
	// AND its literal value is scrubbed out of every free-text section, because
	// the daemon log or a doctor message may quote it.
	Secret
)

// fieldSeparator joins the Go field names of a leaf path ("Download.VPN.Enabled").
// Go names, not TOML keys, because the reflection walk sees Go names and a
// mismatch between the two would make a stale map entry look present.
const fieldSeparator = "."

// leafPaths returns the dotted Go path of every settable leaf field reachable
// from v, in sorted order. Nested structs (and pointers to structs) are walked
// through; everything else — including []string, *bool and time.Duration — is a
// leaf, because a leaf is "a thing a human has to make a publish/redact
// decision about", not "a thing with no fields".
//
// Unexported fields are skipped: the TOML decoder cannot fill them, so they
// hold no user input (config.Config.unknownKeys is the only one, and it holds
// key NAMES the user typed, not values — see collectConfigIssues).
func leafPaths(t reflect.Type) []string {
	var out []string
	walkLeaves(t, "", func(path string, _ reflect.StructField) {
		out = append(out, path)
	})
	sort.Strings(out)
	return out
}

// walkLeaves is the shared recursion behind leafPaths and the test helper that
// fills a Config with sentinels. Sharing it is deliberate: if the walk and the
// filler could disagree about what a leaf is, the sentinel test would quietly
// stop covering the fields the classification test insists on.
func walkLeaves(t reflect.Type, prefix string, fn func(path string, f reflect.StructField)) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported
			continue
		}
		path := f.Name
		if prefix != "" {
			path = prefix + fieldSeparator + f.Name
		}
		if inner, ok := structUnder(f.Type); ok {
			walkLeaves(inner, path, fn)
			continue
		}
		fn(path, f)
	}
}

// structUnder unwraps a pointer once and reports whether what is left is a
// struct we should descend into. time.Time and friends are structs too, but the
// config schema holds none of them today; if one is ever added, the walk would
// descend into its unexported guts and the classification test would fail
// loudly — which is the correct way to find out.
func structUnder(t reflect.Type) (reflect.Type, bool) {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil, false
	}
	return t, true
}

// secretPaths lists the classified-Secret leaves, sorted. Used to build the
// literal scrubber and by the tests.
func secretPaths() []string {
	out := make([]string, 0, len(configFields))
	for path, s := range configFields {
		if s == Secret {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// valueAt resolves a dotted leaf path against a concrete value. Returns the
// zero Value when any pointer on the way is nil, so a caller can range over
// secretPaths() without special-casing an unset *bool.
func valueAt(v reflect.Value, path string) reflect.Value {
	for _, name := range strings.Split(path, fieldSeparator) {
		if v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return reflect.Value{}
			}
			v = v.Elem()
		}
		if v.Kind() != reflect.Struct {
			return reflect.Value{}
		}
		v = v.FieldByName(name)
		if !v.IsValid() {
			return reflect.Value{}
		}
	}
	return v
}

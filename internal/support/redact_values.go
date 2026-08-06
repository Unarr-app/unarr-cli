package support

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// How a Publishable string reaches the bundle.
//
// None of these ever return the caller's bytes unless those bytes are
// something WE defined: a value from a closed vocabulary, or a value that
// matches a shape we wrote. Everything else collapses to a constant. That is
// what makes the redaction an allowlist all the way down — not just at the
// field level but at the value level — and it is why a secret pasted into the
// wrong config key (a password typed into `agent.name`, say) still cannot ride
// out in the bundle.
const (
	valueUnset       = "<unset>"
	valueSet         = "<set>"
	valueDefault     = "<default>"
	valueCustom      = "<custom>"
	valueNonStandard = "<non-standard>"
)

// defaultAPIURL mirrors config.Default()'s auth.api_url. Duplicated as a
// literal rather than imported because the question here is "is this the value
// we ship?", and a future default change must not silently reclassify every
// old install's custom URL as "<default>".
const defaultAPIURL = "https://unarr.app"

// presence is the fallback for free-form strings: paths, names, command lines,
// anything the user typed. "Is it configured at all?" answers most support
// questions; the value itself answers none of them and carries the user's
// home directory, hostname or account name.
func presence(v string) string {
	if strings.TrimSpace(v) == "" {
		return valueUnset
	}
	return valueSet
}

// orUnset publishes v exactly as configured. It is the ONE escape from the
// allowlist above, and the only field wired to it is agent.id — an identifier
// the server minted, not something the user typed, so there is no free text to
// leak. Do not reach for this for anything a human authored: use presence().
func orUnset(v string) string {
	if strings.TrimSpace(v) == "" {
		return valueUnset
	}
	return strings.TrimSpace(v)
}

// pick publishes v verbatim when it is one of the values we documented, and
// flags anything else as non-standard. A typo'd enum is a real support finding
// — "<non-standard>" says "this key is set to something we do not recognise"
// without repeating what the user wrote.
func pick(v string, allowed ...string) string {
	if strings.TrimSpace(v) == "" {
		return valueUnset
	}
	if slices.Contains(allowed, strings.ToLower(strings.TrimSpace(v))) {
		return strings.ToLower(strings.TrimSpace(v))
	}
	return valueNonStandard
}

// picks is pick over a list, preserving order and length. Length is itself
// information (an empty preferred_methods behaves very differently from a
// single-entry one), so an unrecognised entry becomes a placeholder rather
// than disappearing.
func picks(vs []string, allowed ...string) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, pick(v, allowed...))
	}
	return out
}

// Shapes we authored, used to publish machine-ish values verbatim.
//
// A value that matches one of these cannot be a credential: it is a duration,
// a byte size, a bitrate or a language tag. A value that does not match is
// published as "<non-standard>", which is exactly the answer support wants
// when someone writes stall_timeout = "30 minutes".
var (
	durationShape = regexp.MustCompile(`^\d+(\.\d+)?(ns|us|ms|s|m|h|d)?$`)
	sizeShape     = regexp.MustCompile(`^(?i)\d+(\.\d+)?\s*(b|k|m|g|t|kb|mb|gb|tb|kib|mib|gib|tib)?$`)
	bitrateShape  = regexp.MustCompile(`^(?i)\d+(\.\d+)?\s*(k|m|kb|mb|kbps|mbps)?$`)
	langShape     = regexp.MustCompile(`^(?i)[a-z]{2,3}([-_][a-z]{2,4})?$`)
	regionShape   = regexp.MustCompile(`^(?i)[a-z]{2,3}([-_][a-z]{2,4})?$`)
)

// shaped publishes v only when it matches a shape we defined.
func shaped(v string, re *regexp.Regexp) string {
	t := strings.TrimSpace(v)
	if t == "" {
		return valueUnset
	}
	if re.MatchString(t) {
		return t
	}
	return valueNonStandard
}

// shapedList is shaped over a list.
func shapedList(vs []string, re *regexp.Regexp) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, shaped(v, re))
	}
	return out
}

// endpoint answers "stock or custom?" for a server URL. The custom hostname is
// withheld: a self-hosted endpoint is often an internal name or a home IP, and
// the fact that it is non-default is the whole diagnostic.
func endpoint(v string) string {
	switch strings.TrimSpace(strings.TrimSuffix(v, "/")) {
	case "":
		return valueUnset
	case defaultAPIURL:
		return valueDefault
	default:
		return valueCustom
	}
}

// count publishes how many entries a list has instead of the entries. Used for
// mirrors and CORS origins, where "the user added 3 extra origins" is the
// finding and the origins themselves are their private hostnames.
func count(vs []string) string {
	return strconv.Itoa(len(vs)) + " configured"
}

// tribool renders an opt-in/opt-out *bool. nil is not "false": these keys mean
// "the user has not decided", and the daemon's default for an unset key is not
// always the zero value — collapsing the two would send support looking for a
// setting the user never made.
func tribool(b *bool) string {
	if b == nil {
		return valueUnset
	}
	return strconv.FormatBool(*b)
}

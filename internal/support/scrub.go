package support

import (
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

// redactedMarker replaces anything the scrubber catches.
const redactedMarker = "[REDACTED]"

// minLiteralLen is the shortest secret value worth erasing from free text.
//
// Below it, erasing does more harm than good: a 4-character WebDAV username
// like "bob" occurs inside ordinary words and paths, and a scrubber that
// rewrites "bobbin.mkv" has corrupted the evidence the bundle exists to carry.
// Short credentials are still withheld from config.redacted.toml — this bound
// applies only to the free-text pass.
const minLiteralLen = 6

// keyPrefixLen is how many leading characters of a long credential we ALSO
// erase. `unarr doctor` prints the first 8 characters of the API key as a
// "yes, a key is configured" confirmation, and that string lands in doctor.json
// verbatim. Eight characters of an API key is a weak leak rather than a fatal
// one, but the bundle is meant to be attachable without thinking, so it goes.
const keyPrefixLen = 8

// Scrubber erases credentials from free text.
//
// Two passes, and they are not redundant:
//
//   - Literals: the actual values of the Secret-classified config fields. This
//     is the exact pass — it catches a credential wherever it appears, in any
//     shape, including a bare occurrence in a log line that no pattern would
//     recognise. It works because we hold the values.
//   - Patterns: secrets we do NOT hold. Stream tokens are minted at runtime,
//     Authorization headers are echoed by HTTP debug lines, and a WireGuard key
//     may be quoted by an error from a file we never parsed. Nothing enumerable
//     exists for these, so shape matching is the only tool.
//
// The pattern pass is a denylist and is treated as one: it is a second net
// under the allowlist, never the thing the design rests on. The config file —
// the one place credentials are guaranteed to live — is protected by
// publishedConfig, which cannot miss a field it does not know about.
type Scrubber struct {
	literals []string
	patterns []*regexp.Regexp
}

// secretPatterns are the shapes of credentials that never pass through Config.
//
// Each one replaces the SECRET, not the whole match, so the surrounding line
// stays readable — a log line that becomes "[REDACTED]" teaches nothing.
var secretPatterns = []*regexp.Regexp{
	// key=value / key: value for the credential-ish key names. The 8-character
	// floor keeps `require_stream_token = true` (and every other boolean or
	// small number that happens to sit under a matching key) intact.
	// The value class excludes '[' and '<' so this pass cannot rewrite a marker
	// an earlier pass (or the config projection) already put there: without it,
	// "api_key=[REDACTED]" gains a second bracket and `api_key = "<withheld>"`
	// turns into "[REDACTED]", losing the deliberate distinction between a key
	// that is set and one that was never configured.
	regexp.MustCompile(`(?i)\b(api[_-]?key|apikey|agent[_-]?hash|stream[_-]?secret|private[_-]?key|password|passwd|secret)\b(\s*[:=]\s*"?)([^\s"',;&)}<>\[\]]{8,})`),
	// The stream token unarr embeds in playback URLs (?t=… / &t=…).
	regexp.MustCompile(`([?&]t=)([^&\s"'<>)]{8,})`),
	// Authorization: Bearer <token>, however it was logged.
	regexp.MustCompile(`(?i)(bearer\s+)([A-Za-z0-9._\-+/=]{8,})`),
	// A bare 44-character base64 blob with the trailing '=' — the exact shape of
	// a WireGuard key, and of most 32-byte secrets rendered for a config file.
	regexp.MustCompile(`([A-Za-z0-9+/]{43}=)`),
}

// NewScrubber builds the scrubber for one bundle run. It reads the values of
// every field classified Secret — by path, through reflection — so a newly
// classified credential starts being erased from logs with no further wiring.
func NewScrubber(c config.Config) *Scrubber {
	s := &Scrubber{patterns: secretPatterns}
	v := reflect.ValueOf(c)
	for _, path := range secretPaths() {
		fv := valueAt(v, path)
		if !fv.IsValid() || fv.Kind() != reflect.String {
			continue
		}
		s.addLiteral(fv.String())
	}
	// Longest first, so a value that contains another (a key and its own
	// 8-character prefix) is erased whole instead of leaving a tail behind.
	sort.Slice(s.literals, func(i, j int) bool { return len(s.literals[i]) > len(s.literals[j]) })
	return s
}

// addLiteral records a secret value and, for a long one, the truncated form
// the doctor report prints.
func (s *Scrubber) addLiteral(v string) {
	v = strings.TrimSpace(v)
	if len(v) < minLiteralLen {
		return
	}
	s.literals = append(s.literals, v)
	if len(v) > keyPrefixLen {
		s.literals = append(s.literals, v[:keyPrefixLen])
	}
}

// Text erases every known credential from b and returns the result. Safe on
// JSON and TOML payloads: the replacement carries no quote, brace or backslash,
// so a scrubbed document still parses.
func (s *Scrubber) Text(b []byte) []byte {
	out := string(b)
	for _, lit := range s.literals {
		out = strings.ReplaceAll(out, lit, redactedMarker)
	}
	for _, re := range s.patterns {
		out = re.ReplaceAllStringFunc(out, func(m string) string {
			return keepPrefix(re, m)
		})
	}
	return []byte(out)
}

// keepPrefix rebuilds a pattern match with everything but its last capture
// group preserved. Every pattern in secretPatterns puts the secret in the final
// group, so this is the one rule that covers all of them — and it is why a
// scrubbed line still says WHICH key was redacted.
func keepPrefix(re *regexp.Regexp, match string) string {
	g := re.FindStringSubmatch(match)
	if len(g) < 2 {
		return redactedMarker
	}
	return strings.Join(g[1:len(g)-1], "") + redactedMarker
}

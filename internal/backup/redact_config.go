package backup

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

// configSecret is one credential in config.toml: the table that holds it, its
// key, and the field it decodes into — the field is how redactConfig PROVES the
// value is gone instead of trusting the line rewrite.
//
// The list mirrors the Secret verdicts in internal/support (configFields), and
// TestRedactedConfigCarriesNoSupportSecret fails when the two drift apart. One
// deliberate exception: [mount] directory is Secret there (a support bundle is
// published), but it is a local mount point, not a credential — like
// downloads.dir, a restore that dropped it would not restore the setup.
// VPN WireGuard keys are absent too: the config only stores a PATH to the .conf
// (vpn.config_file), and that file is outside the backup.
type configSecret struct {
	table, key string
	field      func(*config.Config) *string
}

var configSecrets = []configSecret{
	{"auth", "api_key", func(c *config.Config) *string { return &c.Auth.APIKey }},
	{"agent", "agent_hash", func(c *config.Config) *string { return &c.Agent.Hash }},
	{"downloads", "webdav_username", func(c *config.Config) *string { return &c.Download.WebDAVUsername }},
	{"downloads", "webdav_password", func(c *config.Config) *string { return &c.Download.WebDAVPassword }},
}

func (s configSecret) id() string { return s.table + "." + s.key }

// RedactedConfigKeys names the config.toml keys a secret-free archive blanks.
func RedactedConfigKeys() []string {
	out := make([]string, len(configSecrets))
	for i, s := range configSecrets {
		out[i] = s.id()
	}
	return out
}

var (
	// tableHeaderRe matches "[name]" / "[[name]]" with an optional comment.
	tableHeaderRe = regexp.MustCompile(`^[ \t]*\[\[?[ \t]*([A-Za-z0-9_.-]+)[ \t]*\]\]?[ \t]*(?:#.*)?$`)
	// keyValueRe matches a bare key set to a ONE-LINE basic or literal string,
	// anchored to the end of the line. The anchor is what keeps a multi-line
	// `"""…"""` or `'''…'''` value from matching as an empty "" or '' prefix
	// (which would leave the secret behind, or corrupt the TOML).
	keyValueRe = regexp.MustCompile(`^([ \t]*([A-Za-z0-9_-]+)[ \t]*=[ \t]*)("(?:[^"\\]|\\.)*"|'[^']*')([ \t]*(?:#.*)?)$`)
)

// rewriteSecrets calls fn with the raw quoted value of every secret key found
// in its own table, and replaces the value with what fn returns. Line-based on
// purpose: round-tripping through a TOML encoder would drop the user's
// comments and ordering. Forms it does not recognise (dotted keys, inline
// tables, multi-line strings) are left alone — redactConfig catches those.
func rewriteSecrets(data []byte, fn func(s configSecret, quoted string) string) []byte {
	lines := strings.SplitAfter(string(data), "\n")
	table := ""
	for i, line := range lines {
		body := strings.TrimRight(line, "\r\n")
		if m := tableHeaderRe.FindStringSubmatch(body); m != nil {
			table = m[1]
			continue
		}
		m := keyValueRe.FindStringSubmatch(body)
		if m == nil {
			continue
		}
		for _, s := range configSecrets {
			if s.table == table && s.key == m[2] {
				lines[i] = m[1] + fn(s, m[3]) + m[4] + line[len(body):]
				break
			}
		}
	}
	return []byte(strings.Join(lines, ""))
}

func isEmptyString(quoted string) bool { return quoted == `""` || quoted == `''` }

// redactConfig blanks every secret value, keeping the key so the file stays
// valid TOML, then decodes the result and FAILS CLOSED if any secret still
// decodes to a value: a config written in a form the line rewrite missed is a
// refused backup (use --encrypt), never a leaked credential.
func redactConfig(data []byte) ([]byte, error) {
	out := rewriteSecrets(data, func(configSecret, string) string { return `""` })
	var c config.Config
	if _, err := toml.Decode(string(out), &c); err != nil {
		return nil, fmt.Errorf("%w: config.toml does not parse: %v", ErrRedaction, err)
	}
	for _, s := range configSecrets {
		if *s.field(&c) != "" {
			return nil, fmt.Errorf("%w: %s is written in a form unarr cannot blank", ErrRedaction, s.id())
		}
	}
	return out, nil
}

// mergeConfigSecrets copies the secret values of the CURRENT config into a
// redacted one being restored, so restoring a redacted archive never signs the
// user out by blanking the api_key that is already in place. It returns the ids
// of the credentials set on this machine that it could not carry over.
func mergeConfigSecrets(restored, current []byte) ([]byte, []string) {
	have := map[string]string{}
	rewriteSecrets(current, func(s configSecret, quoted string) string {
		if !isEmptyString(quoted) {
			have[s.id()] = quoted
		}
		return quoted
	})
	merged := rewriteSecrets(restored, func(s configSecret, quoted string) string {
		if v, ok := have[s.id()]; ok && isEmptyString(quoted) {
			return v
		}
		return quoted // the archive carries a real value: keep it
	})
	var cur, got config.Config
	if _, err := toml.Decode(string(current), &cur); err != nil {
		return merged, nil // nothing reliable to compare against
	}
	if _, err := toml.Decode(string(merged), &got); err != nil {
		return merged, nil // Plan reports the archive's own problems
	}
	var lost []string
	for _, s := range configSecrets {
		if *s.field(&cur) != "" && *s.field(&got) == "" {
			lost = append(lost, s.id())
		}
	}
	return merged, lost
}

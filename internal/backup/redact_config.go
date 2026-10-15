package backup

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

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

// agentIDKey is [agent] id. Not a secret, but the identity this machine's
// api_key and agent_hash belong to (see keepLocalAgentID).
var agentIDKey = configSecret{"agent", "id", func(c *config.Config) *string { return &c.Agent.ID }}

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

// rewriteSecrets is rewriteKeys over every configSecret.
func rewriteSecrets(data []byte, fn func(s configSecret, quoted string) string) []byte {
	return rewriteKeys(data, configSecrets, fn)
}

// rewriteKeys calls fn with the raw quoted value of every key in keys found in
// its own table, and replaces the value with what fn returns. Line-based on
// purpose: round-tripping through a TOML encoder would drop the user's
// comments and ordering. Forms it does not recognise (dotted keys, inline
// tables, multi-line strings) are left alone — redactConfig catches those.
func rewriteKeys(data []byte, keys []configSecret, fn func(s configSecret, quoted string) string) []byte {
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
		for _, s := range keys {
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
	c, err := decodeConfig(out)
	if err != nil {
		return nil, fmt.Errorf("%w: config.toml does not parse: %v", ErrRedaction, err)
	}
	for _, s := range configSecrets {
		if *s.field(&c) != "" {
			return nil, fmt.Errorf("%w: %s is written in a form unarr cannot blank", ErrRedaction, s.id())
		}
	}
	return out, nil
}

// hasCredentials reports whether config data sets any credential; false when it
// does not parse.
func hasCredentials(data []byte) bool {
	c, err := decodeConfig(data)
	if err != nil {
		return false
	}
	for _, s := range configSecrets {
		if *s.field(&c) != "" {
			return true
		}
	}
	return false
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
	cur, err := decodeConfig(current)
	if err != nil {
		return merged, nil // nothing reliable to compare against
	}
	got, err := decodeConfig(merged)
	if err != nil {
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

// mergeRedactedConfig fits a secret-free config.toml being restored onto the
// current one: the credentials already here stay (mergeConfigSecrets), and so
// does the agent id they belong to (keepLocalAgentID). It returns the config
// to write and the warnings for the plan — among them any new server those
// credentials would be sent to (endpointWarnings).
func mergeRedactedConfig(restored, current []byte) ([]byte, []string) {
	merged, lost := mergeConfigSecrets(restored, current)
	var warn []string
	for _, id := range lost {
		warn = append(warn, fmt.Sprintf("%s is set on this machine but cannot be carried over: set it again after restoring", id))
	}
	merged, note := keepLocalAgentID(merged, current)
	if note != "" {
		warn = append(warn, note)
	}
	return merged, append(warn, endpointWarnings(merged, current)...)
}

// keepLocalAgentID stops a secret-free restore from pairing the archive's agent
// id with this machine's credentials: the api_key that mergeConfigSecrets keeps
// was minted for THIS machine's id (`unarr login` binds it), and agent_hash is
// this machine's too. So while the machine has either credential, its id stays
// — even an empty one (a config written by hand, a sign-in that never
// finished): the archive's id is left out rather than handed to credentials it
// does not belong to. Without credentials the archive's id is restored as
// before — the replacement-machine path, where `unarr login` follows. It
// returns the config to write and a note for the plan ("" when the id was not
// in question).
func keepLocalAgentID(restored, current []byte) ([]byte, string) {
	cur, err := decodeConfig(current)
	if err != nil {
		return restored, ""
	}
	arc, err := decodeConfig(restored)
	if err != nil {
		return restored, ""
	}
	if cur.Agent.ID == arc.Agent.ID || (cur.Auth.APIKey == "" && cur.Agent.Hash == "") {
		return restored, ""
	}
	out := setAgentID(restored, quotedAgentID(current, cur.Agent.ID))
	if got, err := decodeConfig(out); err != nil || got.Agent.ID != cur.Agent.ID {
		return restored, fmt.Sprintf("the archive's agent id %q is written in a form unarr cannot replace, so it is restored next to "+
			"this machine's credentials: run `unarr login` after restoring", arc.Agent.ID)
	}
	if cur.Agent.ID == "" {
		return out, fmt.Sprintf("left [agent] id empty instead of restoring the archive's %q: the api_key and agent_hash here "+
			"belong to this machine, not to that agent. Run `unarr login` after restoring", arc.Agent.ID)
	}
	return out, fmt.Sprintf("kept this machine's agent id %q (the archive has %q): the api_key and agent_hash here belong to it. "+
		"Run `unarr login` if this is a new machine", cur.Agent.ID, arc.Agent.ID)
}

// quotedAgentID is the [agent] id exactly as current writes it, or id quoted
// afresh when the line rewrite does not recognise its form.
func quotedAgentID(current []byte, id string) string {
	quoted := strconv.Quote(id)
	rewriteKeys(current, []configSecret{agentIDKey}, func(_ configSecret, q string) string {
		quoted = q
		return q
	})
	return quoted
}

// setAgentID sets [agent] id to quoted, adding the key — and the table — when
// the config has none (written by hand, or before the machine signed in).
func setAgentID(data []byte, quoted string) []byte {
	found := false
	out := rewriteKeys(data, []configSecret{agentIDKey}, func(configSecret, string) string {
		found = true
		return quoted
	})
	if found {
		return out
	}
	line := "id = " + quoted + "\n"
	lines := strings.SplitAfter(string(data), "\n")
	for i, l := range lines {
		if m := tableHeaderRe.FindStringSubmatch(strings.TrimRight(l, "\r\n")); m != nil && m[1] == "agent" {
			if !strings.HasSuffix(l, "\n") {
				line = "\n" + line // the header is the last line
			}
			return []byte(strings.Join(lines[:i+1], "") + line + strings.Join(lines[i+1:], ""))
		}
	}
	s := string(data)
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return []byte(s + "\n[agent]\n" + line)
}

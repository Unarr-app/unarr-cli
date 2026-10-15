package backup

import (
	"fmt"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

// endpoints is where unarr sends a config's api_key and agent_hash: auth.api_url,
// and auth.mirrors when that cannot be reached.
type endpoints struct {
	apiURL  string
	mirrors []string
}

// endpointsOf resolves the endpoints of config data the way config.Load does:
// decoded over config.Default(), so a missing api_url or mirrors keeps the
// default, and a blank api_url means config.DefaultAPIURL. ok is false when
// the data does not parse.
func endpointsOf(data []byte) (e endpoints, ok bool) {
	c := config.Default()
	if _, err := toml.Decode(string(data), &c); err != nil {
		return endpoints{}, false
	}
	e = endpoints{apiURL: c.Auth.APIURL, mirrors: c.Auth.Mirrors}
	if strings.TrimSpace(e.apiURL) == "" {
		e.apiURL = config.DefaultAPIURL
	}
	return e, true
}

// sameURL compares two endpoints, ignoring a trailing slash.
func sameURL(a, b string) bool { return strings.TrimRight(a, "/") == strings.TrimRight(b, "/") }

// endpointWarnings warns when a secret-free restore would send the credentials
// it keeps somewhere new. They stay because they are this machine's, but
// auth.api_url and auth.mirrors come from the archive — a server they were
// never issued to, should the archive have been tampered with. A warning, not
// a refusal: the daemon accepts any URL there, and pointing a machine at
// another server is a real use. merged is the config about to be written.
// When this machine's own config does not parse there is nothing to compare
// against, and the plan says so rather than staying silent.
func endpointWarnings(merged, current []byte) []string {
	got, err := decodeConfig(merged)
	if err != nil || (got.Auth.APIKey == "" && got.Agent.Hash == "") {
		return nil // no credentials kept: nothing to send anywhere
	}
	arc, ok := endpointsOf(merged)
	if !ok {
		return nil // Plan refuses a merged config that does not parse
	}
	cur, ok := endpointsOf(current)
	if !ok {
		return []string{fmt.Sprintf("this machine's config.toml does not parse, so unarr cannot tell whether auth.api_url or "+
			"auth.mirrors change: the api_key/agent_hash kept here will be sent to the archive's auth.api_url %q (mirrors: %s): "+
			"restore only if you trust that server, or set auth.api_url and auth.mirrors after restoring", arc.apiURL, quoteAll(arc.mirrors))}
	}
	var warn []string
	if !sameURL(arc.apiURL, cur.apiURL) {
		warn = append(warn, fmt.Sprintf("auth.api_url changes from %q to the archive's %q, and the api_key/agent_hash kept here "+
			"will be sent there: restore only if you trust that server, or set auth.api_url back after restoring", cur.apiURL, arc.apiURL))
	}
	if added := newMirrors(arc, cur); len(added) > 0 {
		warn = append(warn, fmt.Sprintf("auth.mirrors gains %s from the archive, and the api_key/agent_hash kept here will be sent "+
			"there whenever auth.api_url is unreachable: restore only if you trust them, or set auth.mirrors back after restoring",
			strings.Join(added, ", ")))
	}
	return warn
}

// plainArchiveWarnings warns when an unencrypted archive that carries its own
// credentials would sign this machine in as someone else, or point it at
// another server. Nothing in such an archive is authenticated — the checksums
// prove integrity, not who wrote it — so whoever had the file could have
// edited it, manifest included: a secret-free archive relabelled "included"
// gets past checkRedacted. Pre-restore snapshots are written exactly so, which
// is why it warns instead of refusing, and says nothing when the archive's
// credentials are this machine's own (its snapshot, restored).
func plainArchiveWarnings(archived, current []byte) []string {
	arc, err := decodeConfig(archived)
	if err != nil {
		return nil // checkConfig refuses it
	}
	arcEP, _ := endpointsOf(archived)
	const lead = "this archive is not encrypted, so whoever had the file could have edited it, checksums and all: "
	const tail = ": restore only an archive you made, or run `unarr login` after restoring"
	cur, err := decodeConfig(current)
	if err != nil {
		if !hasIdentity(arc) {
			return nil
		}
		return []string{fmt.Sprintf(lead+"this machine's config.toml does not parse, so unarr cannot tell whether the archive's "+
			"api_key/agent_hash and auth.api_url %q are this machine's"+tail, arcEP.apiURL)}
	}
	if hasIdentity(arc) && arc.Auth.APIKey == cur.Auth.APIKey && arc.Agent.Hash == cur.Agent.Hash {
		return nil // this machine's own identity
	}
	changes := foreignCredentials(arc, cur)
	if curEP, _ := endpointsOf(current); !sameURL(arcEP.apiURL, curEP.apiURL) {
		changes = append(changes, fmt.Sprintf("auth.api_url changes from %q to %q", curEP.apiURL, arcEP.apiURL))
	}
	if len(changes) == 0 {
		return nil
	}
	return []string{lead + strings.Join(changes, "; ") + tail}
}

func hasIdentity(c config.Config) bool { return c.Auth.APIKey != "" || c.Agent.Hash != "" }

// foreignCredentials words each credential the archive sets that differs from
// this machine's.
func foreignCredentials(arc, cur config.Config) []string {
	var out []string
	if arc.Auth.APIKey != "" && arc.Auth.APIKey != cur.Auth.APIKey {
		out = append(out, "its auth.api_key is not this machine's, so the machine signs in as whoever it belongs to")
	}
	if arc.Agent.Hash != "" && arc.Agent.Hash != cur.Agent.Hash {
		out = append(out, "its agent.agent_hash is not this machine's")
	}
	return out
}

// quoteAll words a list of URLs for a warning; "none" when it is empty.
func quoteAll(urls []string) string {
	if len(urls) == 0 {
		return "none"
	}
	out := make([]string, len(urls))
	for i, u := range urls {
		out[i] = fmt.Sprintf("%q", u)
	}
	return strings.Join(out, ", ")
}

// newMirrors lists the archive's mirrors this machine sends nothing to yet:
// in neither its mirrors nor its api_url. A reordered list adds none.
func newMirrors(arc, cur endpoints) []string {
	var out []string
	for _, m := range arc.mirrors {
		known := sameURL(m, cur.apiURL) || slices.ContainsFunc(cur.mirrors, func(c string) bool { return sameURL(m, c) })
		if !known {
			out = append(out, fmt.Sprintf("%q", m))
		}
	}
	return out
}

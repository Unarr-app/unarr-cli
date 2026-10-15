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
func endpointWarnings(merged, current []byte) []string {
	got, err := decodeConfig(merged)
	if err != nil || (got.Auth.APIKey == "" && got.Agent.Hash == "") {
		return nil // no credentials kept: nothing to send anywhere
	}
	cur, ok := endpointsOf(current)
	arc, ok2 := endpointsOf(merged)
	if !ok || !ok2 {
		return nil
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

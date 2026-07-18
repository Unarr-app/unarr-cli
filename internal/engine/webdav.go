package engine

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"log"
	"net/http"

	"golang.org/x/net/webdav"
)

// Read-only WebDAV export of the agent's organized library.
//
// This mounts golang.org/x/net/webdav's standard Handler under /dav/ on the
// SAME StreamServer mux (so it inherits the bound port, graceful shutdown, and
// the per-agent HTTPS listener), fronted by a guard that (1) rejects every
// mutating verb so the export is strictly read-only, and (2) requires HTTP
// Basic auth. Unlike /stream and /hls — whose tokens exist because a <video>
// tag can't send an Authorization header (see stream_token.go) — WebDAV clients
// (Infuse/Kodi/VLC/rclone) send Basic auth natively, so /dav/ uses Basic and is
// exempt from the stream-token machinery.
//
// The Basic password is DERIVED FROM THE STABLE API KEY (DeriveWebDAVPassword),
// never from the daemon's streamSecret — that secret is regenerated on every
// start, which would silently break every mounted Infuse/Kodi share on restart.

const (
	// webdavPrefix is the URL subtree the library is served under. The mux mounts
	// webdavPrefix+"/"; the Handler strips webdavPrefix so paths resolve at "/".
	webdavPrefix = "/dav"

	// webdavRealm labels the Basic auth challenge.
	webdavRealm = "unarr"

	// webdavAllowHeader advertises the read-only verb set on a 405/OPTIONS.
	webdavAllowHeader = "OPTIONS, GET, HEAD, PROPFIND"

	// webdavDerivationLabel is the fixed HMAC message that salts the derived
	// credential, so the derivation can be rotated later (bump the suffix)
	// without colliding with old shares. Not itself a secret.
	webdavDerivationLabel = "unarr-webdav-v1"

	// webdavPassLen is how many hex chars of the HMAC to expose as the credential.
	webdavPassLen = 24
)

// EnableWebDAV arms the read-only WebDAV export. Call before Listen(); Listen()
// mounts it on the mux when enabled. The FileSystem must be read-only (davfs.FS)
// — the guard enforces read-only at the HTTP layer regardless, but a mutating
// FileSystem would still be wrong. username/password are the Basic credentials.
func (ss *StreamServer) EnableWebDAV(fs webdav.FileSystem, username, password string) {
	ss.webdavHandler = &webdav.Handler{
		Prefix:     webdavPrefix,
		FileSystem: fs,
		LockSystem: webdav.NewMemLS(), // required by the Handler; LOCK/UNLOCK are 405'd upstream
		Logger: func(r *http.Request, err error) {
			if err != nil {
				log.Printf("[webdav] %s %s: %v", r.Method, r.URL.Path, err)
			}
		},
	}
	ss.webdavUserHash = sha256.Sum256([]byte(username))
	ss.webdavPassHash = sha256.Sum256([]byte(password))
	ss.webdavEnabled = true
}

// WebDAVEnabled reports whether the read-only WebDAV export is armed.
func (ss *StreamServer) WebDAVEnabled() bool { return ss.webdavEnabled }

// webdavGuard wraps the webdav.Handler with the read-only + Basic-auth contract.
func (ss *StreamServer) webdavGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !webdavMethodAllowed(r.Method) {
			w.Header().Set("Allow", webdavAllowHeader)
			http.Error(w, "read-only WebDAV: method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Answer OPTIONS ourselves instead of delegating: the stock webdav.Handler
		// advertises `DAV: 1, 2` and an Allow list with PUT/DELETE/COPY/MOVE/LOCK,
		// contradicting the read-only contract (those verbs are 405'd). Advertise
		// only the read verbs. Unauthenticated, like the stock handler — capability
		// discovery, no data. Class 1 only (no LOCK → no class 2).
		if r.Method == http.MethodOptions {
			w.Header().Set("Allow", webdavAllowHeader)
			w.Header().Set("DAV", "1")
			w.Header().Set("MS-Author-Via", "DAV")
			w.WriteHeader(http.StatusOK)
			return
		}
		if !ss.webdavAuthOK(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="`+webdavRealm+`", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Never let a caching proxy/CDN store a directory listing or media body
		// served behind Basic auth.
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// webdavMethodAllowed permits only the read verbs. Every mutating verb (PUT,
// DELETE, MKCOL, COPY, MOVE, PROPPATCH, LOCK, UNLOCK, POST) is rejected with 405
// BEFORE it reaches the handler — the read-only guarantee does not depend on the
// FileSystem's own error mapping.
func webdavMethodAllowed(method string) bool {
	switch method {
	case http.MethodOptions, http.MethodGet, http.MethodHead, "PROPFIND":
		return true
	default:
		return false
	}
}

// webdavAuthOK verifies Basic credentials in constant time. Both the username
// and password are compared (as SHA-256 digests, so the compare inputs are
// fixed-length) and the results ANDed without short-circuiting, so neither
// timing nor length leaks which field was wrong.
func (ss *StreamServer) webdavAuthOK(r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	gotUser := sha256.Sum256([]byte(user))
	gotPass := sha256.Sum256([]byte(pass))
	userOK := subtle.ConstantTimeCompare(gotUser[:], ss.webdavUserHash[:])
	passOK := subtle.ConstantTimeCompare(gotPass[:], ss.webdavPassHash[:])
	return userOK&passOK == 1
}

// ResolveWebDAVCreds is the single source of truth for the effective WebDAV
// Basic credentials: username defaults to "unarr", password falls back to the
// key-derived one. active reports whether a usable password exists — both the
// daemon (whether to arm the mount) and `unarr status` (whether to print the
// block) MUST branch on it so status never advertises a mount the daemon
// refused to start. Takes the raw fields (not config.Config) so engine doesn't
// import config.
func ResolveWebDAVCreds(username, password, apiKey string) (user, pass string, active bool) {
	user = username
	if user == "" {
		user = "unarr"
	}
	pass = password
	if pass == "" {
		pass = DeriveWebDAVPassword(apiKey)
	}
	return user, pass, pass != ""
}

// DeriveWebDAVPassword computes a stable WebDAV password from the agent's API
// key: hex(HMAC-SHA256(key=apiKey, msg="unarr-webdav-v1"))[:24]. It is stable
// across daemon restarts (so mounted shares keep working), deterministically
// recomputable by `unarr status` for display, and one-way (does not leak the
// API key). An empty API key yields an empty password (the caller should set an
// explicit webdav_password in that case).
func DeriveWebDAVPassword(apiKey string) string {
	if apiKey == "" {
		return ""
	}
	m := hmac.New(sha256.New, []byte(apiKey))
	m.Write([]byte(webdavDerivationLabel))
	return hex.EncodeToString(m.Sum(nil))[:webdavPassLen]
}

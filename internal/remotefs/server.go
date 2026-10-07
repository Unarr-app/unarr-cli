package remotefs

import (
	"crypto/sha256"
	"crypto/subtle"
	"mime"
	"net/http"
	"path"
	"strings"

	"golang.org/x/net/webdav"
)

// Handler returns an authenticated read-only WebDAV endpoint. The caller must
// bind it to loopback (or put it behind its own authenticated TLS transport).
func Handler(fs *FS, username, password string) http.Handler {
	return handlerWithCache(fs, username, password, newPropCache())
}

func handlerWithCache(fs *FS, username, password string, cache *propCache) http.Handler {
	dav := &webdav.Handler{Prefix: "/dav", FileSystem: fs, LockSystem: webdav.NewMemLS()}
	userHash, passHash := sha256.Sum256([]byte(username)), sha256.Sum256([]byte(password))
	reads := make(chan struct{}, 32)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/dav/") {
			http.NotFound(w, r)
			return
		}
		if !mountAuth(r, userHash, passHash, password != "") {
			w.Header().Set("WWW-Authenticate", `Basic realm="unarr-mount", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Cache-Control", "private, no-cache")
		if r.Method == "PROPFIND" {
			r = r.WithContext(r.Context())
		}
		if !allowDAVMethod(w, r) {
			return
		}
		if r.Method == "PROPFIND" && cache != nil {
			cache.serve(w, r, dav, fs.current.Load())
			return
		}
		serveDAVRead(w, r, dav, reads)
	})
}

func serveDAVRead(w http.ResponseWriter, r *http.Request, dav http.Handler, reads chan struct{}) {
	if r.Method == http.MethodGet {
		select {
		case reads <- struct{}{}:
			defer func() { <-reads }()
			w = progressWriter{ResponseWriter: w, timeout: mediaIdleTimeout}
		default:
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many active reads", http.StatusServiceUnavailable)
			return
		}
	}
	setDAVMediaHeaders(w, r)
	r = r.WithContext(withRange(r.Context(), r.Header.Get("Range")))
	dav.ServeHTTP(w, r)
}

func setDAVMediaHeaders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return
	}
	// Avoid the 512-byte sniff before a tail seek, including unknown extensions.
	contentType := mime.TypeByExtension(path.Ext(r.URL.Path))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

func mountAuth(r *http.Request, userHash, passHash [32]byte, active bool) bool {
	user, pass, ok := r.BasicAuth()
	u, p := sha256.Sum256([]byte(user)), sha256.Sum256([]byte(pass))
	return ok && active && subtle.ConstantTimeCompare(u[:], userHash[:])&subtle.ConstantTimeCompare(p[:], passHash[:]) == 1
}

func allowDAVMethod(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Allow", "OPTIONS, GET, HEAD, PROPFIND")
	switch r.Method {
	case http.MethodOptions:
		w.Header().Set("DAV", "1")
		w.WriteHeader(http.StatusOK)
		return false
	case "PROPFIND":
		if d := r.Header.Get("Depth"); d != "0" && d != "1" {
			http.Error(w, "use Depth 0 or 1", http.StatusForbidden)
			return false
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	case http.MethodGet, http.MethodHead:
	default:
		http.Error(w, "read-only filesystem", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

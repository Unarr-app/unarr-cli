package engine

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/net/webdav"
)

// TestEnableWebDAVArmsAuth ties the arming path to the auth check: EnableWebDAV
// flips WebDAVEnabled()==true and stores the sha256 user/pass hashes such that
// webdavAuthOK accepts exactly those credentials and rejects anything else.
func TestEnableWebDAVArmsAuth(t *testing.T) {
	ss := NewStreamServer(0, 1)
	if ss.WebDAVEnabled() {
		t.Fatal("WebDAVEnabled() = true before EnableWebDAV")
	}

	ss.EnableWebDAV(webdav.Dir(t.TempDir()), "alice", "s3cr3t-pass")
	if !ss.WebDAVEnabled() {
		t.Fatal("WebDAVEnabled() = false after EnableWebDAV")
	}

	authReq := func(user, pass string, set bool) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/dav/x.mkv", nil)
		if set {
			r.SetBasicAuth(user, pass)
		}
		return r
	}

	if !ss.webdavAuthOK(authReq("alice", "s3cr3t-pass", true)) {
		t.Error("webdavAuthOK rejected the exact armed credentials")
	}
	if ss.webdavAuthOK(authReq("alice", "wrong", true)) {
		t.Error("webdavAuthOK accepted a wrong password")
	}
	if ss.webdavAuthOK(authReq("mallory", "s3cr3t-pass", true)) {
		t.Error("webdavAuthOK accepted a wrong username")
	}
	if ss.webdavAuthOK(authReq("", "", false)) {
		t.Error("webdavAuthOK accepted a request with no Basic credentials")
	}
}

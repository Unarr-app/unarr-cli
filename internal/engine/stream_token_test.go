package engine

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStreamToken_RoundTrip(t *testing.T) {
	secret := newStreamSecret()
	now := time.Now()
	tok := mintStreamToken(secret, streamScopeStream, now)
	if !verifyStreamToken(secret, streamScopeStream, tok, now) {
		t.Fatalf("freshly minted token failed to verify: %q", tok)
	}
	// Still valid just before expiry.
	if !verifyStreamToken(secret, streamScopeStream, tok, now.Add(streamTokenTTL-time.Minute)) {
		t.Error("token rejected before its TTL elapsed")
	}
}

func TestStreamToken_Expired(t *testing.T) {
	secret := newStreamSecret()
	now := time.Now()
	tok := mintStreamToken(secret, streamScopeStream, now)
	if verifyStreamToken(secret, streamScopeStream, tok, now.Add(streamTokenTTL+time.Second)) {
		t.Error("expired token verified as valid")
	}
}

func TestStreamToken_WrongScope(t *testing.T) {
	secret := newStreamSecret()
	now := time.Now()
	tok := mintStreamToken(secret, streamScopeHLS("abc"), now)
	if verifyStreamToken(secret, streamScopeStream, tok, now) {
		t.Error("token for one scope verified under another")
	}
	if verifyStreamToken(secret, streamScopeHLS("xyz"), tok, now) {
		t.Error("hls token verified for a different session id")
	}
	if !verifyStreamToken(secret, streamScopeHLS("abc"), tok, now) {
		t.Error("hls token failed to verify under its own session id")
	}
}

func TestStreamToken_WrongSecret(t *testing.T) {
	now := time.Now()
	tok := mintStreamToken(newStreamSecret(), streamScopeStream, now)
	if verifyStreamToken(newStreamSecret(), streamScopeStream, tok, now) {
		t.Error("token verified under a different secret")
	}
}

func TestStreamToken_Tampered(t *testing.T) {
	secret := newStreamSecret()
	now := time.Now()
	tok := mintStreamToken(secret, streamScopeStream, now)
	// Flip the last hex char of the MAC.
	last := tok[len(tok)-1]
	flip := byte('0')
	if last == '0' {
		flip = '1'
	}
	tampered := tok[:len(tok)-1] + string(flip)
	if verifyStreamToken(secret, streamScopeStream, tampered, now) {
		t.Error("tampered MAC verified as valid")
	}
}

func TestStreamToken_Malformed(t *testing.T) {
	secret := newStreamSecret()
	now := time.Now()
	for _, bad := range []string{
		"",
		"nodot",
		"123.",         // empty MAC
		".deadbeef",    // empty exp
		"notanint.abc", // non-numeric exp
		".",
	} {
		if verifyStreamToken(secret, streamScopeStream, bad, now) {
			t.Errorf("malformed token %q verified as valid", bad)
		}
	}
}

// TestVerifyStreamToken_CrossLangVector pins the wire format against the web's
// TypeScript minter (tests/unit/stream-token.test.ts asserts the same vector).
// A token the web mints MUST verify here or remote HLS playback 404s.
func TestVerifyStreamToken_CrossLangVector(t *testing.T) {
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = 0xab // matches secretHex "ab"*32 on the web side
	}
	const (
		sessionID = "sess-1"
		token     = "1900000000.3ee840ccf2c2a42b784d7cef68458db1d3cea5ecdcab41061504de32eb52fbc2"
	)
	before := time.Unix(1899978400, 0) // before exp 1900000000
	if !verifyStreamToken(secret, streamScopeHLS(sessionID), token, before) {
		t.Fatal("web-minted parity token failed to verify in the daemon")
	}
	after := time.Unix(1900000001, 0) // past exp
	if verifyStreamToken(secret, streamScopeHLS(sessionID), token, after) {
		t.Error("parity token verified past its expiry")
	}
}

func TestNewStreamSecret_LengthAndUniqueness(t *testing.T) {
	a, b := newStreamSecret(), newStreamSecret()
	if len(a) != 32 {
		t.Errorf("secret length = %d, want 32", len(a))
	}
	if string(a) == string(b) {
		t.Error("two secrets were identical — not random")
	}
}

// --- /stream handler enforcement ---------------------------------------------

func streamReq(remoteAddr, query string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://stream.test/stream"+query, nil)
	r.RemoteAddr = remoteAddr
	return r
}

func newServedServer(t *testing.T) *StreamServer {
	t.Helper()
	srv := NewStreamServer(0, 1)
	srv.SetFile(newFakeProvider("movie.mkv", []byte("fake video bytes")), "task-1")
	return srv
}

func TestStreamHandler_RemoteWithoutToken_404(t *testing.T) {
	srv := newServedServer(t)
	rec := httptest.NewRecorder()
	srv.handler(rec, streamReq("198.51.100.7:40000", ""))
	if rec.Code != http.StatusNotFound {
		t.Errorf("remote request without token: status = %d, want 404", rec.Code)
	}
}

func TestStreamHandler_RemoteValidToken_200(t *testing.T) {
	srv := newServedServer(t)
	tok := mintStreamToken(srv.streamSecret, streamScopeStream, time.Now())
	rec := httptest.NewRecorder()
	srv.handler(rec, streamReq("198.51.100.7:40000", "?t="+tok))
	if rec.Code != http.StatusOK {
		t.Errorf("remote request with valid token: status = %d, want 200", rec.Code)
	}
}

func TestStreamHandler_RemoteBadToken_404(t *testing.T) {
	srv := newServedServer(t)
	rec := httptest.NewRecorder()
	srv.handler(rec, streamReq("198.51.100.7:40000", "?t=deadbeef.0000"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("remote request with bad token: status = %d, want 404", rec.Code)
	}
}

func TestStreamHandler_LoopbackWithoutToken_404(t *testing.T) {
	// No loopback exemption: cloudflared relays public funnel traffic over
	// localhost, so loopback must still present a valid token.
	srv := newServedServer(t)
	rec := httptest.NewRecorder()
	srv.handler(rec, streamReq("127.0.0.1:55555", ""))
	if rec.Code != http.StatusNotFound {
		t.Errorf("loopback request without token: status = %d, want 404 (no exemption)", rec.Code)
	}
}

func TestStreamHandler_LoopbackWithValidToken_200(t *testing.T) {
	srv := newServedServer(t)
	tok := mintStreamToken(srv.streamSecret, streamScopeStream, time.Now())
	rec := httptest.NewRecorder()
	srv.handler(rec, streamReq("127.0.0.1:55555", "?t="+tok))
	if rec.Code != http.StatusOK {
		t.Errorf("loopback request with valid token: status = %d, want 200", rec.Code)
	}
}

func TestStreamHandler_EnforcementOff_NoToken_200(t *testing.T) {
	srv := newServedServer(t)
	srv.SetRequireStreamToken(false)
	rec := httptest.NewRecorder()
	srv.handler(rec, streamReq("198.51.100.7:40000", ""))
	if rec.Code != http.StatusOK {
		t.Errorf("enforcement off: status = %d, want 200", rec.Code)
	}
}

// --- /hls handler enforcement ------------------------------------------------

func TestHLSHandler_RemoteBadToken_404(t *testing.T) {
	srv := NewStreamServer(0, 1)
	// A syntactically valid session id (UUID-ish) with a bogus token segment.
	const sess = "11111111-1111-4111-8111-111111111111"
	r := httptest.NewRequest(http.MethodGet, "http://stream.test/hls/"+sess+"/badtoken/master.m3u8", nil)
	r.RemoteAddr = "198.51.100.7:40000"
	rec := httptest.NewRecorder()
	srv.hlsHandler(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Errorf("remote hls with bad token: status = %d, want 404", rec.Code)
	}
}

func TestHLSBaseURLs_CarryTokenSegment(t *testing.T) {
	srv := NewStreamServer(0, 1)
	srv.urls.LAN = "http://192.168.1.2:11818/stream"
	const sess = "22222222-2222-4222-8222-222222222222"
	urls := srv.hlsBaseURLs(sess)
	prefix := "http://192.168.1.2:11818/hls/" + sess + "/"
	if !strings.HasPrefix(urls.LAN, prefix) || len(urls.LAN) <= len(prefix) {
		t.Errorf("hls LAN url = %q, want token segment after %q", urls.LAN, prefix)
	}
	// The trailing segment must be a verifiable hls-scoped token.
	tok := strings.TrimPrefix(urls.LAN, prefix)
	if !verifyStreamToken(srv.streamSecret, streamScopeHLS(sess), tok, time.Now()) {
		t.Errorf("token segment %q does not verify for session %s", tok, sess)
	}
}

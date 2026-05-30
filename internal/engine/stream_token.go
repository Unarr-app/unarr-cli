package engine

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// Stream authentication.
//
// /stream and /hls have no header-based auth: a <video src> cannot attach an
// Authorization header, and media-tag/segment requests are issued by the
// browser, not our JS. So we bind a short-lived, unforgeable token to each
// stream URL the daemon hands out and verify it on every request.
//
// The token is HMAC-signed by the daemon's own in-memory secret — there is no
// server-side token store and no DB column. The web is a pure pass-through: it
// stores and serves whatever tokenised URL the agent reports.
//
//   - /stream (+ VLC playlist): token rides as a `?t=` query parameter.
//   - /hls: token rides as a PATH segment — /hls/<sessionID>/<token>/<resource>
//     — so the relative child URIs inside the playlists (video/index.m3u8,
//     seg-N.m4s, subs/…) resolve under the same prefix and carry the token
//     automatically, with zero playlist rewriting.
//
// There is NO loopback exemption: the Cloudflare funnel proxies public traffic
// to the daemon over localhost (cloudflared --url http://localhost:<port>), so
// a loopback source address is NOT a trust signal — exempting it would leave the
// funnel (the headline public path) wide open. Every URL the agent/web hands a
// player is already tokenised (URL(), URLsJSON, buildHlsUrls), so enforcing the
// token unconditionally breaks no legitimate client. /health stays ungated (a
// reachability probe that leaks nothing sensitive).

const (
	// streamTokenTTL is how long a minted token stays valid. Long enough for a
	// movie plus pauses; short enough that a leaked URL stops working same-day.
	streamTokenTTL = 6 * time.Hour

	// streamScopeStream is the token scope for the single-file /stream endpoint.
	streamScopeStream = "stream"
)

// streamScopeHLS is the token scope for an HLS session. Binding to the session
// id means a token minted for one session never validates another.
func streamScopeHLS(sessionID string) string { return "hls:" + sessionID }

// newStreamSecret returns 32 cryptographically-random bytes used to sign stream
// tokens for the lifetime of the daemon. Regenerated each start, so tokens from
// a previous run stop validating (the web re-resolves the URL on demand).
func newStreamSecret() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand.Read does not fail on supported platforms. If it ever
		// does, fail hard rather than fall back to a predictable key while still
		// claiming to enforce auth — a guessable key is worse than no streaming.
		panic("unarr: crypto/rand unavailable, cannot generate stream secret: " + err.Error())
	}
	return b
}

// mintStreamToken issues `<expUnix>.<hexHMAC>` binding scope to an expiry.
// Verification needs only the same secret + scope.
func mintStreamToken(secret []byte, scope string, now time.Time) string {
	expStr := strconv.FormatInt(now.Add(streamTokenTTL).Unix(), 10)
	return expStr + "." + streamTokenMAC(secret, scope, expStr)
}

func streamTokenMAC(secret []byte, scope, expStr string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(scope + ":" + expStr))
	return hex.EncodeToString(m.Sum(nil))
}

// verifyStreamToken reports whether token is a valid, unexpired signature for
// scope under secret. Cheap rejects (format, expiry) happen before the
// constant-time MAC compare since they don't depend on the secret.
func verifyStreamToken(secret []byte, scope, token string, now time.Time) bool {
	dot := strings.IndexByte(token, '.')
	if dot <= 0 || dot >= len(token)-1 {
		return false
	}
	expStr, gotMAC := token[:dot], token[dot+1:]
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || now.Unix() > exp {
		return false
	}
	wantMAC := streamTokenMAC(secret, scope, expStr)
	return subtle.ConstantTimeCompare([]byte(gotMAC), []byte(wantMAC)) == 1
}

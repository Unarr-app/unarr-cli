package cmd

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// streamCacheID is the persistent HLS-cache identity of a URL stream session.
// The info_hash when there is one (debrid links change on every resolution, the
// hash does not); else, for a provider (IPTV) URL — stable per title, with no
// hash — a digest of the URL. "" when neither exists: the engine then refuses to
// cache instead of keying every such title on the same (empty) identity.
func streamCacheID(sess agent.StreamSession) string {
	if sess.InfoHash != "" {
		return sess.InfoHash
	}
	if sess.DirectURL != "" {
		sum := sha256.Sum256([]byte(sess.DirectURL))
		return "url:" + hex.EncodeToString(sum[:])
	}
	return ""
}

// usenetStreamCacheID keys a Usenet stream by its info_hash, else its NZB id.
// Never by the loopback URL it is read through: that URL is per-session.
func usenetStreamCacheID(sess agent.StreamSession) string {
	if sess.InfoHash != "" {
		return sess.InfoHash
	}
	if sess.NzbID != "" {
		return "nzb:" + sess.NzbID
	}
	return ""
}

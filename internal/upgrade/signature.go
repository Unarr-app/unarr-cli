package upgrade

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// releasePubKeyBase64 is the base64-encoded ed25519 public key used to verify
// `checksums.txt.sig` against `checksums.txt` during self-update.
//
// It is overridable at link time via ldflags so the same source compiles for
// users who do not yet have a release-signing keypair in their CI:
//
//	-X github.com/torrentclaw/unarr/internal/upgrade.releasePubKeyBase64=<base64-pubkey>
//
// When the variable is empty, signature verification is skipped and a warning
// is logged — checksum-only verification remains in force. This is the
// transitional default until the keypair is provisioned; flip to a non-empty
// value (and enable the corresponding CI signing step) to make signature
// verification mandatory.
var releasePubKeyBase64 = ""

// ErrMissingSignature indicates the release does not ship a `.sig` file even
// though signature verification is required by an embedded public key.
var ErrMissingSignature = errors.New("release signature file is missing")

// verifyChecksumsSignature downloads `checksums.txt.sig` (raw 64-byte ed25519
// signature over the checksums.txt content) and verifies it with the embedded
// public key. Returns nil if verification succeeds or if no public key has
// been embedded yet (caller is expected to surface a warning in that case).
func verifyChecksumsSignature(ctx context.Context, version string, checksumsContent []byte) error {
	pubKey, err := loadReleasePubKey()
	if err != nil {
		return fmt.Errorf("load release pubkey: %w", err)
	}
	if pubKey == nil {
		// Signature verification not configured; caller decides what to do.
		return nil
	}

	url := releaseURL(version, "checksums.txt.sig")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "unarr-updater")
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch signature: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrMissingSignature
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch signature: HTTP %d", resp.StatusCode)
	}

	// Signature file is base64(signature)\n — small and bounded.
	rawSig, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
	if err != nil {
		return fmt.Errorf("read signature: %w", err)
	}
	sig, err := decodeSignature(rawSig)
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("signature size %d, expected %d", len(sig), ed25519.SignatureSize)
	}
	if !ed25519.Verify(pubKey, checksumsContent, sig) {
		return errors.New("ed25519 signature verification failed")
	}
	return nil
}

// SignatureVerificationConfigured reports whether the build has a release
// public key embedded. The CLI surfaces this so users running a non-signed
// build get a clear warning rather than silent trust.
func SignatureVerificationConfigured() bool {
	pubKey, err := loadReleasePubKey()
	return err == nil && pubKey != nil
}

func loadReleasePubKey() (ed25519.PublicKey, error) {
	v := strings.TrimSpace(releasePubKeyBase64)
	if v == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("pubkey size %d, expected %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// decodeSignature parses the base64-encoded signature emitted by
// scripts/sign-checksums (always base64 + trailing newline). A single
// expected format keeps the surface area minimal — a stricter parser is
// less likely to accept a hostile mirror's coincidentally-sized payload.
func decodeSignature(raw []byte) ([]byte, error) {
	return base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
}

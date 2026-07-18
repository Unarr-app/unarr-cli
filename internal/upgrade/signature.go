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
// It is the canonical release-signing public key, compiled in so every build
// (local ship.sh and CI alike) verifies updates consistently — it is public, so
// committing it is safe and removes any "forgot to set the env var → shipped an
// unsigned/unverifying binary" failure mode. The matching PRIVATE key signs
// checksums.txt during release (scripts/sign-checksums, driven by the
// goreleaser `signs:` block); releases are signed unconditionally now.
//
// When this is empty, signature verification is skipped (a warning is logged).
// Do NOT clear it — every release from v1.0.1-beta on ships a checksums.txt.sig
// and clients built with this key require it. Rotating the key is a coordinated
// change: clients on the old key must update before the signing key flips.
var releasePubKeyBase64 = "X7EJVwAiIILs4EGaqp+YBsa4Q6HnKBB2J5FI4MIt+w0="

// ErrMissingSignature indicates the release does not ship a `.sig` file even
// though signature verification is required by an embedded public key.
var ErrMissingSignature = errors.New("release signature file is missing")

// sigAssetName derives a manifest's signature asset name. The ".sig" suffix
// is a release-pipeline invariant (scripts/sign-checksums and desktop.yml both
// emit "<manifest>.sig"), so deriving it here — instead of naming the two
// files independently at every call site — makes a manifest/sig mismatch
// unrepresentable.
func sigAssetName(manifest string) string {
	return manifest + ".sig"
}

// verifySignedChecksums is the manifest-agnostic core of the signature check:
// the CLI verifies checksums.txt, the desktop updater checksums-desktop.txt —
// each against its derived "<manifest>.sig" (sigAssetName) — both with the
// SAME compiled-in release public key, so one signing secret in CI covers
// every artifact class. Returns nil if verification succeeds or if no public
// key has been embedded (caller is expected to surface a warning then).
func verifySignedChecksums(ctx context.Context, version, base, manifest string, checksumsContent []byte) error {
	pubKey, err := loadReleasePubKey()
	if err != nil {
		return fmt.Errorf("load release pubkey: %w", err)
	}
	if pubKey == nil {
		// Signature verification not configured; caller decides what to do.
		return nil
	}

	rawSig, err := fetchSignature(ctx, version, base, sigAssetName(manifest))
	if err != nil {
		return err
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

// fetchSignature downloads a signature asset (base64(signature)\n — small and
// bounded) from `base`: the SAME mirror that served the archive + checksums
// manifest (resolved by download). Fetching all three from one host is what
// keeps verification sound — the two builds (GitHub Actions and local ship)
// are not guaranteed byte-identical, so a signature from one mirror would NOT
// verify a checksums manifest from another. A 404 means the release on this
// mirror genuinely ships no signature → ErrMissingSignature (the CLI's
// --allow-unsigned path; a HARD error for desktop assets); other non-200s and
// transport errors surface as-is.
func fetchSignature(ctx context.Context, version, base, sigAsset string) ([]byte, error) {
	url := releaseURL(base, version, sigAsset)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "unarr-updater")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch signature: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		if rerr != nil {
			return nil, fmt.Errorf("read signature: %w", rerr)
		}
		return data, nil
	case http.StatusNotFound:
		return nil, ErrMissingSignature
	default:
		return nil, fmt.Errorf("fetch signature: HTTP %d", resp.StatusCode)
	}
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

package upgrade

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// withReleasePubKey temporarily swaps the embedded release public key and
// restores the previous value on test exit.
func withReleasePubKey(t *testing.T, encoded string) {
	t.Helper()
	prev := releasePubKeyBase64
	releasePubKeyBase64 = encoded
	t.Cleanup(func() { releasePubKeyBase64 = prev })
}

func TestSignatureVerificationDisabledByDefault(t *testing.T) {
	withReleasePubKey(t, "")
	if SignatureVerificationConfigured() {
		t.Fatal("expected SignatureVerificationConfigured() to be false when pubkey is empty")
	}
	// verifyChecksumsSignature should be a no-op when no key is embedded.
	if err := verifyChecksumsSignature(context.Background(), "0.0.0", []byte("anything")); err != nil {
		t.Fatalf("expected nil when pubkey is empty, got %v", err)
	}
}

func TestSignatureRejectsMalformedPubKey(t *testing.T) {
	withReleasePubKey(t, "not-base64!!")
	if _, err := loadReleasePubKey(); err == nil {
		t.Fatal("expected error from malformed base64")
	}
}

func TestSignatureRejectsWrongSizePubKey(t *testing.T) {
	withReleasePubKey(t, base64.StdEncoding.EncodeToString([]byte("too-short")))
	if _, err := loadReleasePubKey(); err == nil {
		t.Fatal("expected error from wrong-size pubkey")
	}
}

func TestSignatureVerifiesGoodSignature(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate keypair: %v", err)
	}
	withReleasePubKey(t, base64.StdEncoding.EncodeToString(pub))

	checksumsBody := []byte("deadbeef  unarr_0.0.0_linux_amd64.tar.gz\n")
	signature := ed25519.Sign(priv, checksumsBody)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "checksums.txt.sig") {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintln(w, base64.StdEncoding.EncodeToString(signature))
	}))
	defer srv.Close()

	prevHost := githubReleaseHost
	githubReleaseHost = srv.URL
	t.Cleanup(func() { githubReleaseHost = prevHost })

	if err := verifyChecksumsSignature(context.Background(), "0.0.0", checksumsBody); err != nil {
		t.Fatalf("verifyChecksumsSignature(good) = %v, want nil", err)
	}
}

func TestSignatureRejectsBadSignature(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate keypair: %v", err)
	}
	withReleasePubKey(t, base64.StdEncoding.EncodeToString(pub))

	// Sign with a DIFFERENT private key — should be rejected.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	body := []byte("checksum-line\n")
	badSig := ed25519.Sign(other, body)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, base64.StdEncoding.EncodeToString(badSig))
	}))
	defer srv.Close()

	prevHost := githubReleaseHost
	githubReleaseHost = srv.URL
	t.Cleanup(func() { githubReleaseHost = prevHost })

	err = verifyChecksumsSignature(context.Background(), "0.0.0", body)
	if err == nil || !strings.Contains(err.Error(), "verification failed") {
		t.Fatalf("expected verification failure, got %v", err)
	}
}

func TestSignatureMissingFile(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	withReleasePubKey(t, base64.StdEncoding.EncodeToString(pub))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	prevHost := githubReleaseHost
	githubReleaseHost = srv.URL
	t.Cleanup(func() { githubReleaseHost = prevHost })

	err := verifyChecksumsSignature(context.Background(), "0.0.0", []byte("body"))
	if !errors.Is(err, ErrMissingSignature) {
		t.Fatalf("expected ErrMissingSignature, got %v", err)
	}
}

func TestDecodeSignatureRejectsRaw(t *testing.T) {
	// 64-byte payload that happens NOT to be valid base64 must error rather
	// than be silently accepted as a raw signature — the only legitimate
	// shape is base64-encoded text.
	raw := make([]byte, ed25519.SignatureSize)
	for i := range raw {
		raw[i] = 0xff
	}
	if _, err := decodeSignature(raw); err == nil {
		t.Fatal("expected error from non-base64 64-byte payload")
	}
}

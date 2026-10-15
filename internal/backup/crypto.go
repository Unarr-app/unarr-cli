package backup

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
)

// Encrypted container: magic | salt(16) | nonce(12) | AES-256-GCM(zip).
// The key is PBKDF2-HMAC-SHA256(passphrase, salt, 600k) — the OWASP figure —
// from the standard library, so no dependency is added. The header is bound as
// GCM additional data, so flipping a header byte fails authentication.
var encMagic = []byte("UNARRBK1")

const (
	saltLen   = 16
	pbkdfIter = 600_000
)

func deriveKey(pass string, salt []byte) ([]byte, error) {
	return pbkdf2.Key(sha256.New, pass, salt, pbkdfIter, 32)
}

func newGCM(pass string, salt []byte) (cipher.AEAD, error) {
	key, err := deriveKey(pass, salt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// isEncrypted reports whether data starts with the container magic.
func isEncrypted(data []byte) bool { return bytes.HasPrefix(data, encMagic) }

func encrypt(plain []byte, pass string) ([]byte, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	gcm, err := newGCM(pass, salt)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	header := append(append(append([]byte{}, encMagic...), salt...), nonce...)
	return gcm.Seal(header, nonce, plain, header), nil
}

func decrypt(data []byte, pass string) ([]byte, error) {
	if pass == "" {
		return nil, ErrPassphraseRequired
	}
	hdrLen := len(encMagic) + saltLen + 12
	if len(data) < hdrLen+16 {
		return nil, fmt.Errorf("%w: truncated", ErrBadPassphrase)
	}
	header := data[:hdrLen]
	salt := header[len(encMagic) : len(encMagic)+saltLen]
	nonce := header[len(encMagic)+saltLen:]
	gcm, err := newGCM(pass, salt)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, data[hdrLen:], header)
	if err != nil {
		return nil, ErrBadPassphrase
	}
	return plain, nil
}

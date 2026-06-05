// Package acme handles the agent side of the per-agent direct-TLS feature
// (plex.direct model). The agent generates and keeps its private key LOCALLY,
// builds a CSR for *.<hash>.agent.unarr.app, and sends only the CSR to the
// web-side broker (which runs the ACME order against Let's Encrypt via DNS-01
// and returns the signed chain). The key never leaves the machine.
//
// File layout under the agent state dir:
//
//	certs/agent.key   ECDSA P-256 private key (PEM, persisted across renewals)
//	certs/agent.crt   issued certificate chain (PEM, hot-reloaded by the stream server)
package acme

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// GenerateHash returns a 32-hex-char (16-byte) high-entropy agent hash label.
func GenerateHash() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate agent hash: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Paths returns the key/cert file paths under the agent state dir.
func Paths(dataDir string) (keyPath, certPath string) {
	dir := filepath.Join(dataDir, "certs")
	return filepath.Join(dir, "agent.key"), filepath.Join(dir, "agent.crt")
}

// loadOrCreateKey returns the agent's persistent EC key, creating + persisting
// it on first use. Reused across renewals so the cert always matches the key.
func loadOrCreateKey(keyPath string) (*ecdsa.PrivateKey, error) {
	if data, err := os.ReadFile(keyPath); err == nil {
		block, _ := pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("agent.key is not valid PEM")
		}
		key, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse agent.key: %w", err)
		}
		return key, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate EC key: %w", err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal EC key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir certs: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(keyPath, pemBytes, 0o600); err != nil {
		return nil, fmt.Errorf("write agent.key: %w", err)
	}
	return key, nil
}

// BuildCSR ensures the persistent key exists and returns a PEM CSR requesting
// the wildcard *.<hash>.<baseDomain> (plus the bare <hash>.<baseDomain> so a
// future non-wildcard use still validates). baseDomain e.g. "agent.unarr.app".
func BuildCSR(dataDir, hash, baseDomain string) (csrPEM string, err error) {
	keyPath, _ := Paths(dataDir)
	key, err := loadOrCreateKey(keyPath)
	if err != nil {
		return "", err
	}
	wildcard := "*." + hash + "." + baseDomain
	base := hash + "." + baseDomain
	tmpl := &x509.CertificateRequest{
		Subject:            pkix.Name{CommonName: wildcard},
		DNSNames:           []string{wildcard, base},
		SignatureAlgorithm: x509.ECDSAWithSHA256,
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		return "", fmt.Errorf("create CSR: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})), nil
}

// WriteCert persists the issued certificate chain atomically (temp file + rename)
// so a concurrent reader (NeedsIssue, or the listener's GetCertificate reload)
// can never observe a half-written PEM during a renewal.
func WriteCert(dataDir, certPEM string) error {
	_, certPath := Paths(dataDir)
	if err := os.MkdirAll(filepath.Dir(certPath), 0o700); err != nil {
		return fmt.Errorf("mkdir certs: %w", err)
	}
	tmp := certPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(certPEM), 0o644); err != nil {
		return fmt.Errorf("write agent.crt: %w", err)
	}
	if err := os.Rename(tmp, certPath); err != nil {
		return fmt.Errorf("rename agent.crt: %w", err)
	}
	return nil
}

// renewBefore is how long ahead of expiry we proactively renew.
const renewBefore = 30 * 24 * time.Hour

// NeedsIssue reports whether we should (re)request a cert: true when the cert is
// missing, unparseable, expired, or within renewBefore of expiry.
func NeedsIssue(dataDir string) bool {
	_, certPath := Paths(dataDir)
	data, err := os.ReadFile(certPath)
	if err != nil {
		return true
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return true
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return true
	}
	return time.Now().Add(renewBefore).After(cert.NotAfter)
}

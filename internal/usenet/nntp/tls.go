package nntp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"net"
)

// dialTLS opens a TLS connection to addr, verified against the configured
// TLSServerName override when there is one.
//
// The override exists for a Host that is a CNAME onto a provider whose
// certificate names the provider, not the CNAME. It is also a single point of
// failure: when reader.torrentclaw.com rotated on 2026-09-10 to a certificate
// valid only for its own names, every agent still told to expect "xsnews.nl"
// failed the handshake — against a certificate that was perfectly valid for the
// host it was actually dialling. So when, and only when, the handshake with the
// override fails because the certificate does not cover that NAME, retry once
// verified against Host. Verification is never relaxed: the fallback checks the
// chain and the name exactly as strictly, and any other failure (untrusted CA,
// expiry) is returned as-is. After a successful fallback the client dials Host
// directly, and it says so in the log once.
func (c *Client) dialTLS(ctx context.Context, dialer *net.Dialer, addr string) (net.Conn, error) {
	name := c.tlsServerName()
	conn, err := c.tlsHandshake(ctx, dialer, addr, name)
	if err == nil || name == c.cfg.Host || !isHostnameMismatch(err) {
		return conn, err
	}
	conn, fbErr := c.tlsHandshake(ctx, dialer, addr, c.cfg.Host)
	if fbErr != nil {
		return nil, fmt.Errorf("tls server name %q: %w; fallback to host name %q: %w", name, err, c.cfg.Host, fbErr)
	}
	c.tlsOverrideStale.Store(true)
	c.tlsFallbackLog.Do(func() {
		log.Printf("[nntp] certificate of %s does not cover TLS server name override %q; verified against the host name instead (fix the credentials' tlsServerName)",
			c.cfg.Host, name)
	})
	return conn, nil
}

// tlsServerName is the name the next handshake verifies: the override, unless it
// is unset or has already been proven not to match the server's certificate.
func (c *Client) tlsServerName() string {
	if c.cfg.TLSServerName == "" || c.tlsOverrideStale.Load() {
		return c.cfg.Host
	}
	return c.cfg.TLSServerName
}

// tlsHandshake dials addr and completes a fully verified TLS handshake for
// serverName. rootCAs is nil (the system pool) outside tests.
func (c *Client) tlsHandshake(ctx context.Context, dialer *net.Dialer, addr, serverName string) (net.Conn, error) {
	d := &tls.Dialer{
		NetDialer: dialer,
		Config: &tls.Config{
			ServerName: serverName,
			MinVersion: tls.VersionTLS12,
			RootCAs:    c.rootCAs,
		},
	}
	return d.DialContext(ctx, "tcp", addr)
}

// isHostnameMismatch reports whether a handshake failed only because the
// certificate is not valid for the requested name (x509.HostnameError, which
// crypto/tls wraps in a tls.CertificateVerificationError).
func isHostnameMismatch(err error) bool {
	var he x509.HostnameError
	if errors.As(err, &he) {
		return true
	}
	var hp *x509.HostnameError
	return errors.As(err, &hp)
}

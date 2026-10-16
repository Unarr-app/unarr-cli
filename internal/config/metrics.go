package config

import (
	"fmt"
	"net"
)

// MetricsConfig gates the optional Prometheus endpoint. Off by default: a
// disabled feature binds no port and runs no goroutine for /metrics.
//
// Publish is separate: it sends the session view to the web (POST
// /api/internal/agent/sessions) and is ON by default, whatever Enabled says.
type MetricsConfig struct {
	Enabled bool `toml:"enabled"`
	// Publish sends the session snapshot to the web. false = nothing is sent.
	Publish bool `toml:"publish"`
	// Listen is the host:port to bind. Default 127.0.0.1:9877 (loopback).
	Listen string `toml:"listen,omitempty"`
	// Token is the bearer token scrapers must present. Empty = reuse the
	// control-plane token, which is only allowed on a loopback bind (it is
	// regenerated on every daemon start).
	Token string `toml:"token,omitempty"`
}

// Address returns the bind address, defaulting to loopback.
func (m MetricsConfig) Address() string {
	if m.Listen == "" {
		return "127.0.0.1:9877"
	}
	return m.Listen
}

// Loopback reports whether the bind address is a loopback IP.
func (m MetricsConfig) Loopback() bool {
	host, _, err := net.SplitHostPort(m.Address())
	ip := net.ParseIP(host)
	return err == nil && ip != nil && ip.IsLoopback()
}

// Validate rejects an address that cannot be bound as written, and a
// non-loopback bind without its own token (never expose the endpoint on the
// network protected only by an ephemeral secret).
func (m MetricsConfig) Validate() error {
	if !m.Enabled {
		return nil
	}
	host, port, err := net.SplitHostPort(m.Address())
	if err != nil || net.ParseIP(host) == nil {
		return fmt.Errorf("metrics.listen must be an IP:port")
	}
	if _, err := net.LookupPort("tcp", port); err != nil {
		return fmt.Errorf("invalid metrics.listen port")
	}
	if !m.Loopback() && m.Token == "" {
		return fmt.Errorf("metrics.token is required when metrics.listen is not loopback")
	}
	return nil
}

package agent

import "testing"

// newPortReportDaemon builds the minimum Daemon the port updaters touch: a cfg
// and the sync client's copy of it. Both must move together — they are the two
// places register and sync read the port from (buildRequest vs Register).
func newPortReportDaemon(configured int) *Daemon {
	cfg := DaemonConfig{HTTPSStreamPort: configured}
	return &Daemon{cfg: cfg, sync: &SyncClient{cfg: cfg}}
}

// listenTLS bumps the HTTPS port when the configured one is busy. Reporting the
// config value anyway made the web build every direct-TLS URL against a port
// nothing was listening on — the listener was fine, the advertised address was
// not, so remote playback failed with no visible cause.
func TestUpdateHTTPSStreamPortReportsTheBoundPort(t *testing.T) {
	d := newPortReportDaemon(11819)
	d.UpdateHTTPSStreamPort(11820) // busy → listenTLS bumped it

	if got := d.cfg.HTTPSStreamPort; got != 11820 {
		t.Errorf("register must report the bound port, got %d want 11820", got)
	}
	if got := d.sync.cfg.HTTPSStreamPort; got != 11820 {
		t.Errorf("sync must report the bound port, got %d want 11820", got)
	}
}

// No cert issued → no TLS listener → HTTPSPort() is 0. That must reach the wire
// as 0 rather than the configured port: directTLSWire treats 0 as "clear the
// columns", which is the honest answer when nothing is listening.
func TestUpdateHTTPSStreamPortReportsZeroWhenTLSNeverArmed(t *testing.T) {
	d := newPortReportDaemon(11819)
	d.UpdateHTTPSStreamPort(0)

	if got := d.cfg.HTTPSStreamPort; got != 0 {
		t.Errorf("an unarmed TLS listener must report 0, got %d", got)
	}
	if got := d.sync.cfg.HTTPSStreamPort; got != 0 {
		t.Errorf("sync must report 0 for an unarmed TLS listener, got %d", got)
	}
}

// The common case: the configured port was free, so the bound port equals it and
// the reported value is unchanged.
func TestUpdateHTTPSStreamPortKeepsTheConfiguredPortWhenFree(t *testing.T) {
	d := newPortReportDaemon(11819)
	d.UpdateHTTPSStreamPort(11819)

	if got := d.cfg.HTTPSStreamPort; got != 11819 {
		t.Errorf("got %d want 11819", got)
	}
	if got := d.sync.cfg.HTTPSStreamPort; got != 11819 {
		t.Errorf("sync copy got %d want 11819", got)
	}
}

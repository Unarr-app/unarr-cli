package config

import "testing"

// Log rotation is OPT-IN, and this is the guard on that decision.
//
// The ring went through three redesigns and still carries residual failure
// modes on a file another process may be holding (see the "Deuda abierta"
// section of docs/plans/daemon-log-ownership.md). Shipping it enabled would
// hand every install the unsolved part, so the default is 0 — rotation off, no
// ring mutated on a stock install.
//
// Raising this number back is a product decision with known costs attached, not
// a tidy-up. If you are here because this test failed, read that document
// first: turning rotation on by default re-arms every path this default
// disarms.
func TestLogRotationIsOffByDefault(t *testing.T) {
	if defaultLogMaxSizeMB != 0 {
		t.Fatalf("defaultLogMaxSizeMB = %d, want 0 — rotation is opt-in", defaultLogMaxSizeMB)
	}
	if got := Default().Daemon.LogMaxSizeMB; got != 0 {
		t.Errorf("Default().Daemon.LogMaxSizeMB = %d, want 0 — a freshly written "+
			"config must not enable rotation", got)
	}
	// The three ways a real config reaches applyDefaults: the key absent from an
	// existing [daemon] block, no [daemon] block at all, and an empty file. All
	// three have to land on "off", or an upgrade would silently enable rotation
	// on installs that never asked for it.
	for name, body := range map[string]string{
		"key omitted from [daemon]": "[daemon]\nauto_upgrade = true\n",
		"no [daemon] block":         "[downloads]\nmax_concurrent = 3\n",
		"empty config":              "",
	} {
		if got := loadTOML(t, body).Daemon.LogMaxSizeMB; got != 0 {
			t.Errorf("%s: log_max_size_mb resolved to %d, want 0", name, got)
		}
	}
}

// The opt-in has to actually work: a user who sets the key gets exactly what
// they asked for, and the slot count keeps its own (non-zero) default so the
// ring is a ring the moment rotation is on.
func TestAnExplicitBudgetOptsIn(t *testing.T) {
	cfg := loadTOML(t, "[daemon]\nlog_max_size_mb = 12\n")
	if cfg.Daemon.LogMaxSizeMB != 12 {
		t.Fatalf("log_max_size_mb = %d, want the configured 12", cfg.Daemon.LogMaxSizeMB)
	}
	if cfg.Daemon.LogMaxFiles != defaultLogMaxFiles {
		t.Fatalf("log_max_files = %d, want the default %d — opting into rotation "+
			"must not leave the ring sizeless", cfg.Daemon.LogMaxFiles, defaultLogMaxFiles)
	}
}

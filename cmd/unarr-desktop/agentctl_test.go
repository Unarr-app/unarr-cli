package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// isolatePaths sandboxes BOTH roots the tray reads from.
//
// UNARR_CONFIG_DIR alone is NOT enough: the daemon state file lives under
// config.DataDir() (XDG_DATA_HOME / LOCALAPPDATA / the macOS app-support dir),
// which UNARR_CONFIG_DIR does not redirect. A test that only set
// UNARR_CONFIG_DIR read — and overwrote — the developer's REAL
// ~/.local/share/unarr/daemon.state.json.
func isolatePaths(t *testing.T) {
	t.Helper()
	cfgDir, dataDir := t.TempDir(), t.TempDir()
	t.Setenv("UNARR_CONFIG_DIR", cfgDir)
	t.Setenv("XDG_DATA_HOME", dataDir)  // linux
	t.Setenv("LOCALAPPDATA", dataDir)   // windows
	t.Setenv("XDG_CONFIG_HOME", cfgDir) // linux, when UNARR_CONFIG_DIR is unset
}

// TestCurrentAgentID pins the id-resolution chain behind the "Configure agent
// (web)" deep-link. The tray menu can't be clicked headlessly, so this is where
// the behaviour is actually proven.
func TestCurrentAgentID(t *testing.T) {
	t.Run("no config and no state yields empty", func(t *testing.T) {
		isolatePaths(t)
		if got := currentAgentID(); got != "" {
			t.Fatalf("currentAgentID() = %q, want \"\" (never-registered box must fall back to the generic hub)", got)
		}
	})

	t.Run("reads the id from config.toml", func(t *testing.T) {
		isolatePaths(t)
		writeConfig(t, "cfg-agent-id")

		if got := currentAgentID(); got != "cfg-agent-id" {
			t.Fatalf("currentAgentID() = %q, want %q", got, "cfg-agent-id")
		}
	})

	t.Run("falls back to the state file when config has no id", func(t *testing.T) {
		isolatePaths(t)
		writeState(t, "state-agent-id")

		if got := currentAgentID(); got != "state-agent-id" {
			t.Fatalf("currentAgentID() = %q, want %q (running daemon, unreadable config)", got, "state-agent-id")
		}
	})

	// The regression that motivated config-first precedence. Only LockPath is
	// scoped to the config dir — deliberately, so a dev agent and the
	// production agent can run CONCURRENTLY. They then SHARE one
	// daemon.state.json (DataDir is not config-scoped), last writer wins. A
	// tray launched with UNARR_CONFIG_DIR=~/.config/unarr-dev must still
	// identify the DEV agent, not whichever daemon last touched the state file.
	t.Run("config wins over a state file written by another agent", func(t *testing.T) {
		isolatePaths(t)
		writeConfig(t, "dev-agent-id")
		writeState(t, "production-agent-id")

		if got := currentAgentID(); got != "dev-agent-id" {
			t.Fatalf("currentAgentID() = %q, want %q — the shared state file must not "+
				"deep-link the tray to another agent's card", got, "dev-agent-id")
		}
	})
}

func writeConfig(t *testing.T, agentID string) {
	t.Helper()
	body := "[agent]\nid = \"" + agentID + "\"\nname = \"test-box\"\n"
	if err := os.WriteFile(sandboxConfigPath(t), []byte(body), 0o600); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}
}

// writeState fakes a running daemon: readStatus only trusts the state file when
// its PID is alive, so it claims this test process.
func writeState(t *testing.T, agentID string) {
	t.Helper()
	st := agent.DaemonState{
		AgentID:       agentID,
		Status:        "running",
		Version:       "test",
		PID:           os.Getpid(),
		StartedAt:     time.Now(),
		LastHeartbeat: time.Now(),
	}
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	path := agent.StateFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write state file: %v", err)
	}
}

// sandboxConfigPath resolves the sandboxed config path and asserts the sandbox
// actually held — a belt-and-braces guard so a future change to the path
// helpers can never silently point these tests at the developer's real files.
func sandboxConfigPath(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("UNARR_CONFIG_DIR")
	if dir == "" || !filepath.IsAbs(dir) {
		t.Fatalf("test sandbox not in effect: UNARR_CONFIG_DIR=%q", dir)
	}
	return filepath.Join(dir, "config.toml")
}

//go:build linux

package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Frozen e5e8b4c installer output, independent of the migration implementation.
// Only the disposable CLI executable and private HOME are substituted.
const nativeLegacySystemdTemplate = `[Unit]
Description=unarr download daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s start
Restart=always
RestartSec=10
SuccessExitStatus=78
RestartPreventExitStatus=78
Environment=HOME=%s

[Install]
WantedBy=default.target
`

const nativeSystemdOwnedPolicy = "# Managed by unarr for remote mount cleanup.\n[Service]\nKillMode=mixed\n"
const nativeSystemdSentinelBody = "synthetic non-policy note: uninstall must preserve unrelated bytes\n"

type nativeLinuxSystemdPolicy struct {
	base, policy, sentinel string
	legacy                 bool
	original               []byte
	sentinelCreated        bool
}

func newNativeLinuxSystemdPolicy(t *testing.T, base string, legacy bool) *nativeLinuxSystemdPolicy {
	t.Helper()
	directory := base + ".d"
	if _, err := os.Lstat(directory); !os.IsNotExist(err) {
		t.Fatalf("refusing existing private policy directory: %s: %v", directory, err)
	}
	return &nativeLinuxSystemdPolicy{
		base: base, policy: filepath.Join(directory, "90-unarr-remote-mount.conf"),
		sentinel: filepath.Join(directory, "native-fixture-preserve-note.txt"), legacy: legacy,
	}
}

func nativeLinuxSystemdSnapshot(t *testing.T, phase string) map[string]string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "--user", "show", "unarr.service",
		"-p", "LoadState", "-p", "ActiveState", "-p", "SubState", "-p", "MainPID",
		"-p", "FragmentPath", "-p", "DropInPaths", "-p", "KillMode", "-p", "KillSignal",
		"-p", "SendSIGKILL", "-p", "TimeoutStopUSec", "-p", "ExecStart").CombinedOutput()
	if err != nil {
		t.Errorf("actual private manager %s: %v %s", phase, err, out)
		return nil
	}
	t.Logf("actual private manager %s:\n%s", phase, strings.TrimSpace(string(out)))
	properties := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Errorf("invalid private manager receipt: %q", line)
			return nil
		}
		if _, exists := properties[key]; exists {
			t.Errorf("duplicate private manager receipt: %q", key)
			return nil
		}
		properties[key] = value
	}
	return properties
}

func (p *nativeLinuxSystemdPolicy) prepare(t *testing.T) {
	t.Helper()
	if !p.legacy {
		return // The actual CLI must install its new base unit itself.
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	cli, err := filepath.EvalSymlinks(os.Getenv("UNARR_NATIVE_CLI"))
	if err != nil || !filepath.IsAbs(cli) {
		t.Fatalf("canonical task executable required: %v", err)
	}
	p.original = []byte(fmt.Sprintf(nativeLegacySystemdTemplate, cli, home))
	if err := os.MkdirAll(filepath.Dir(p.base), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.base, p.original, 0644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "--user", "daemon-reload").CombinedOutput()
	if err != nil {
		t.Fatalf("load frozen legacy unit without activation/policy injection: %v %s", err, out)
	}
	snapshot := nativeLinuxSystemdSnapshot(t, "before actual legacy migration")
	if snapshot["LoadState"] != "loaded" || snapshot["FragmentPath"] != p.base ||
		snapshot["DropInPaths"] != "" || snapshot["KillMode"] != "control-group" ||
		snapshot["ActiveState"] != "inactive" || snapshot["MainPID"] != "0" {
		t.Fatalf("frozen legacy unit must be canonical inactive control-group without overrides: %v", snapshot)
	}
	p.assertBase(t, "before actual legacy migration")
}

func (p *nativeLinuxSystemdPolicy) assertBase(t *testing.T, phase string) bool {
	t.Helper()
	body, err := os.ReadFile(p.base)
	if err != nil || !bytes.Equal(body, p.original) {
		t.Errorf("private base bytes changed %s: %v", phase, err)
		return false
	}
	t.Logf("unchanged private base %s SHA256=%x legacy=%t", phase, sha256.Sum256(body), p.legacy)
	return true
}

func (p *nativeLinuxSystemdPolicy) active(t *testing.T, phase string) {
	t.Helper()
	snapshot := nativeLinuxSystemdSnapshot(t, phase)
	expectedDropIn := ""
	if p.legacy {
		expectedDropIn = p.policy
		body, err := os.ReadFile(p.policy)
		if err != nil || string(body) != nativeSystemdOwnedPolicy {
			t.Fatalf("actual CLI must create exact owned cleanup policy: %v %q", err, body)
		}
		t.Logf("actual CLI owned policy SHA256=%x body=%q", sha256.Sum256(body), body)
	} else if p.original == nil {
		body, err := os.ReadFile(p.base)
		if err != nil || !strings.Contains(string(body), "\nKillMode=mixed\n") {
			t.Fatalf("actual new installation must include mixed base policy: %v", err)
		}
		p.original = body
	}
	if snapshot["LoadState"] != "loaded" || snapshot["FragmentPath"] != p.base ||
		snapshot["DropInPaths"] != expectedDropIn || snapshot["KillMode"] != "mixed" ||
		snapshot["ActiveState"] != "active" || snapshot["MainPID"] == "0" {
		t.Fatalf("actual private manager must use canonical verified mixed policy %s: %v", phase, snapshot)
	}
	if !p.assertBase(t, phase) {
		t.FailNow()
	}
}

func (p *nativeLinuxSystemdPolicy) preserveSentinel(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p.policy), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.sentinel, []byte(nativeSystemdSentinelBody), 0600); err != nil {
		t.Fatal(err)
	}
	p.sentinelCreated = true
}

func (p *nativeLinuxSystemdPolicy) beforeUninstall(t *testing.T) {
	t.Helper()
	if p.original != nil {
		p.assertBase(t, "before actual uninstall")
	}
}

func (p *nativeLinuxSystemdPolicy) afterUninstall(t *testing.T, davAddress string) bool {
	t.Helper()
	snapshot := nativeLinuxSystemdSnapshot(t, "after actual uninstall")
	if snapshot["LoadState"] != "not-found" || snapshot["MainPID"] != "0" {
		t.Errorf("actual private service survived uninstall: %v", snapshot)
		return false
	}
	for _, path := range []string{p.base, p.policy} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("actual uninstall kept private base/owned policy: %s: %v", path, err)
			return false
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	out, err := exec.CommandContext(ctx, "pgrep", "-x", "rclone").CombinedOutput()
	cancel()
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != 1 {
		t.Errorf("private rclone survived/unverifiable after uninstall: %v %s", err, out)
		return false
	}
	conn, err := net.DialTimeout("tcp", davAddress, time.Second)
	if err == nil {
		_ = conn.Close()
		t.Error("private DAV listener survived actual uninstall")
		return false
	}
	if p.sentinelCreated {
		body, err := os.ReadFile(p.sentinel)
		if err != nil || string(body) != nativeSystemdSentinelBody {
			t.Errorf("actual uninstall changed unrelated non-conf sentinel: %v", err)
			return false
		}
		t.Logf("actual uninstall preserved unrelated sentinel SHA256=%x", sha256.Sum256(body))
		if err := os.Remove(p.sentinel); err != nil {
			t.Error(err)
			return false
		}
	}
	// Remove only the now-empty task-owned directory, never foreign contents.
	if err := os.Remove(filepath.Dir(p.policy)); err != nil && !os.IsNotExist(err) {
		t.Errorf("private policy directory not empty after verified cleanup: %v", err)
		return false
	}
	t.Log("actual uninstall removed base/owned policy and rclone/DAV; unrelated bytes preserved")
	return true
}

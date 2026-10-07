//go:build windows

package cmd

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/winproc"
)

// This uses the disposable account's actual default config and scheduled task.
// The directory guards prevent a fixture from replacing an existing installation.
func TestMountNativeWindowsPersistent(t *testing.T) {
	nativeMountOptIn(t, true)
	if os.Getenv("UNARR_NATIVE_WINDOWS_SERVICE") != "1" {
		t.Skip("disposable Windows account service opt-in required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	query := exec.CommandContext(ctx, "schtasks", "/Query", "/TN", "unarr")
	winproc.HideWindow(query)
	if out, err := query.CombinedOutput(); err == nil {
		t.Fatalf("refusing existing unarr task: %s", out)
	} else if !strings.Contains(strings.ToLower(string(out)), "cannot find") {
		t.Fatalf("task absence could not be verified: %v %s", err, out)
	}
	query = exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", "Get-Process | Where-Object {$_.ProcessName -like 'unarr*' -or $_.ProcessName -eq 'rclone'} | ForEach-Object {$_.Id}")
	winproc.HideWindow(query)
	if out, err := query.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("refusing existing native processes: %v %s", err, out)
	}
	query = exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", "([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)")
	winproc.HideWindow(query)
	if out, err := query.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "False" {
		t.Fatalf("limited Windows token required to preserve firewall policy: %v", err)
	}
	if os.Getenv("UNARR_CONFIG_DIR") != "" {
		t.Fatal("persistent fixture must use actual default service config")
	}
	for _, dir := range []string{config.Dir(), config.DataDir()} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Skipf("existing account data preserved; default directory must be absent: %s", dir)
		}
	}
	f := newNativeMountFixture(t)
	f.cfg.Download.Dir = filepath.Join(t.TempDir(), "downloads")
	if err := os.MkdirAll(f.cfg.Download.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	f.cfg.Download.PreferredMethods = []string{"debrid"}
	f.cfg.Download.HTTPSStreamPort, f.cfg.Download.StreamPort = 0, 0
	f.cfg.Download.AutoHTTPSUpnp, f.cfg.Download.EnableUPnP = false, false
	f.cfg.Download.Transcode.Enabled = false
	f.cfg.Download.Funnel.Enabled, f.cfg.Download.VPN.Enabled = false, false
	disabled := false
	f.cfg.Daemon.AutoUpgrade, f.cfg.Telemetry.Enabled = &disabled, &disabled
	f.cfg.Daemon.Downlink = "poll"
	f.cfg.Agent.ID, f.cfg.Agent.Name = "native-windows-fixture", "Native Windows Fixture"
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.cfg.Mount.Listen = listener.Addr().String()
	_ = listener.Close()
	letter, err := mountDestination(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(letter) != 2 || letter[1] != ':' {
		t.Fatalf("expected genuine Windows drive destination: %q", letter)
	}
	rclone := os.Getenv("UNARR_NATIVE_RCLONE")
	if !filepath.IsAbs(rclone) || filepath.Base(rclone) != "rclone.exe" || !strings.HasSuffix(filepath.Base(filepath.Dir(rclone)), "-windows-amd64") {
		t.Fatal("verified native rclone artifact required")
	}
	t.Cleanup(func() {
		if windowsTaskInstalled() {
			p := startNativeCLI(t, "daemon", "uninstall")
			select {
			case <-p.done:
				if p.err != nil {
					t.Errorf("fixture uninstall: %v %s", p.err, p.output.String())
				}
			case <-time.After(35 * time.Second):
				t.Error("fixture uninstall timed out")
			}
		}
		if windowsTaskInstalled() {
			t.Error("fixture scheduled task survived cleanup")
			return
		}
		if st := agent.ReadState(); st != nil && agent.IsProcessAlive(st.PID) {
			t.Errorf("fixture daemon PID %d survived cleanup", st.PID)
			return
		}
		if _, err := os.Stat(letter + `\`); err == nil {
			t.Error("fixture drive survived cleanup")
			return
		}
		_ = os.RemoveAll(config.Dir())
		_ = os.RemoveAll(config.DataDir())
	})
	// Seed the normal config-local cache from the official checksum-verified
	// artifact. The scheduled task does not inherit this terminal's PATH.
	cache := filepath.Join(config.Dir(), "tools", filepath.Base(filepath.Dir(rclone)))
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(rclone)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "rclone.exe"), data, 0700); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(f.cfg, config.FilePath()); err != nil {
		t.Fatal(err)
	}
	cli := func(args ...string) {
		t.Helper()
		p := startNativeCLI(t, args...)
		select {
		case <-p.done:
			if p.err != nil {
				t.Fatalf("real CLI %v: %v %s", args, p.err, p.output.String())
			}
			t.Logf("CLI %v: %s", args, strings.TrimSpace(p.output.String()))
		case <-time.After(35 * time.Second):
			t.Fatalf("real CLI %v timed out", args)
		}
	}
	cli("mount", letter)
	// The initiating terminal has exited before this mounted read.
	filePath := filepath.Join(letter+`\`, "debrid", "torbox", "Release [123]", "space name.mkv")
	nativeEventually(t, 35*time.Second, "persistent drive after CLI exit", func() bool { _, err := os.Stat(filePath); return err == nil })
	read := func() {
		t.Helper()
		r, err := os.Open(filePath)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		if err := nativeReadSequence(r, f.files["Release [123]/space name.mkv"], 0); err != nil {
			t.Fatal(err)
		}
	}
	read()
	loaded, err := config.Load(config.FilePath())
	if err != nil || loaded.Mount.Directory != letter || loaded.Mount.Validate() != nil {
		t.Fatalf("actual persisted drive config invalid: %v %q", err, loaded.Mount.Directory)
	}
	st := agent.ReadState()
	if st == nil || !agent.IsProcessAlive(st.PID) {
		t.Fatal("persistent daemon state not live")
	}
	oldPID := st.PID
	cli("daemon", "restart")
	nativeEventually(t, 35*time.Second, "daemon restarted with fresh PID", func() bool {
		st := agent.ReadState()
		return st != nil && st.PID != oldPID && st.Status == "running" && agent.IsProcessAlive(st.PID)
	})
	nativeEventually(t, 35*time.Second, "drive reattached after daemon restart", func() bool { _, err := os.Stat(filePath); return err == nil })
	read()
	st = agent.ReadState()
	if st == nil {
		t.Fatal("daemon state disappeared before renewal")
	}
	nativeRenewPersistentIdentity(t, f, filePath, st.PID)
	for _, disableIntent := range []bool{true, false} {
		if !disableIntent {
			cli("mount", letter)
			nativeEventually(t, 35*time.Second, "second persistent drive", func() bool { _, err := os.Stat(filePath); return err == nil })
			read()
		}
		st = agent.ReadState()
		if st == nil || !agent.IsProcessAlive(st.PID) {
			t.Fatal("daemon not live before umount")
		}
		daemonPID := st.PID
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		q := fmt.Sprintf("Get-CimInstance Win32_Process -Filter 'ParentProcessId = %d' | Where-Object {$_.Name -like 'rclone*'} | ForEach-Object {$_.ProcessId}", daemonPID)
		query := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", q)
		winproc.HideWindow(query)
		out, err := query.Output()
		cancel()
		pids := strings.Fields(string(out))
		if err != nil || len(pids) != 1 {
			t.Fatalf("owned persistent rclone before umount: %v %q", err, out)
		}
		rclonePID, err := strconv.Atoi(pids[0])
		if err != nil {
			t.Fatal(err)
		}
		loaded, err = config.Load(config.FilePath())
		if err != nil || !loaded.Mount.Enabled {
			t.Fatalf("mount intent missing before umount: %v", err)
		}
		if disableIntent {
			loaded.Mount.Enabled = false // Simulate the config menu without restarting.
			if err := config.Save(loaded, config.FilePath()); err != nil {
				t.Fatal(err)
			}
		}
		conn, err := net.DialTimeout("tcp", loaded.Mount.Listen, time.Second)
		if err != nil || !agent.IsProcessAlive(rclonePID) {
			t.Fatalf("live mount required before umount (disabled intent=%t): %v", disableIntent, err)
		}
		_ = conn.Close()
		cli("umount")
		nativeEventually(t, 15*time.Second, "drive/DAV/rclone removed after umount", func() bool {
			if _, err := os.Stat(letter + `\`); !os.IsNotExist(err) || agent.IsProcessAlive(rclonePID) {
				return false
			}
			conn, err := net.DialTimeout("tcp", loaded.Mount.Listen, 100*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				return false
			}
			return true
		})
		loaded, err = config.Load(config.FilePath())
		if err != nil || loaded.Mount.Enabled {
			t.Fatalf("umount persistence failed: %v", err)
		}
		nativeEventually(t, 35*time.Second, "healthy ordinary daemon after umount", func() bool {
			st = agent.ReadState()
			return st != nil && st.Status == "running" && agent.IsProcessAlive(st.PID)
		})
		t.Logf("actual native umount disabled intent=%t: drive gone, DAV closed, rclone PID=%d gone, healthy daemon PID=%d (before=%d; explicit umount may restart)", disableIntent, rclonePID, st.PID, daemonPID)
	}
	t.Logf("native Windows persistent drive %s: terminal exit, restart, umount; final daemon PID=%d", letter, st.PID)
}

//go:build linux

package cmd

import (
	"context"
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
)

// Run only inside the separately provisioned disposable native user manager.
// Never set this opt-in on the host with an existing unarr.service.
func TestMountNativeLinuxPersistent(t *testing.T) {
	nativeMountOptIn(t, true)
	if os.Getenv("UNARR_NATIVE_LINUX_SERVICE") != "1" {
		t.Skip("private container/user-manager opt-in required")
	}
	for _, legacy := range []bool{false, true} {
		name := "new-install"
		if legacy {
			name = "recognized-legacy"
		}
		t.Run(name, func(t *testing.T) { nativeLinuxPersistentLifecycle(t, legacy) })
	}
}

func nativeLinuxPersistentLifecycle(t *testing.T, legacy bool) {
	t.Helper()
	if _, err := os.Stat("/.dockerenv"); err != nil || os.Getuid() == 0 {
		t.Fatal("refusing host/root service fixture; disposable non-root Docker user required")
	}
	for _, name := range []string{"UNARR_CONFIG_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "UNARR_API_KEY", "UNARR_API_URL", "UNARR_DOWNLOAD_DIR", "UNARR_COUNTRY", "UNARR_TELEMETRY"} {
		if os.Getenv(name) != "" {
			t.Fatalf("normal service environment required: %s", name)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "--user", "show", "unarr.service", "-p", "LoadState", "--value").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "not-found" {
		t.Fatalf("refusing existing/inaccessible private service: %v %s", err, out)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(home, ".config", "systemd", "user", "unarr.service")
	for _, path := range []string{unit, config.Dir(), config.DataDir()} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("refusing existing private fixture path: %s", path)
		}
	}
	policy := newNativeLinuxSystemdPolicy(t, unit, legacy)
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
	f.cfg.Daemon.Downlink = "poll"
	f.cfg.Agent.ID, f.cfg.Agent.Name = "native-linux-fixture", "Native Linux Fixture"
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.cfg.Mount.Listen = listener.Addr().String()
	_ = listener.Close()
	directory := t.TempDir()
	rclone := os.Getenv("UNARR_NATIVE_RCLONE")
	if !filepath.IsAbs(rclone) || filepath.Base(rclone) != "rclone" || !strings.HasSuffix(filepath.Base(filepath.Dir(rclone)), "-linux-amd64") {
		t.Fatal("official task-local Linux rclone artifact required")
	}
	t.Cleanup(func() {
		nativePersistentFailureDiagnostics(t, f, directory)
		policy.beforeUninstall(t)
		p := startNativeCLI(t, "daemon", "uninstall")
		select {
		case <-p.done:
			if p.err != nil {
				t.Errorf("private service uninstall: %v %s", p.err, p.output.String())
			}
		case <-time.After(35 * time.Second):
			t.Error("private service cleanup timed out")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "systemctl", "--user", "show", "unarr.service", "-p", "LoadState", "--value").CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != "not-found" {
			t.Errorf("private service survived cleanup: %v %s", err, out)
			return
		}
		if st := agent.ReadState(); st != nil && agent.IsProcessAlive(st.PID) {
			t.Errorf("private daemon PID %d survived cleanup", st.PID)
			return
		}
		if _, err := validateMountPoint(directory); err != nil {
			t.Errorf("private mounted destination survived cleanup: %v", err)
			return
		}
		if !policy.afterUninstall(t, f.cfg.Mount.Listen) {
			return
		}
		_ = os.RemoveAll(config.Dir())
		_ = os.RemoveAll(config.DataDir())
	})
	cache := filepath.Join(config.Dir(), "tools", filepath.Base(filepath.Dir(rclone)))
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(rclone)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "rclone"), data, 0700); err != nil {
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
				t.Fatalf("actual private CLI %v: %v %s", args, p.err, p.output.String())
			}
			t.Logf("CLI %v: %s", args, strings.TrimSpace(p.output.String()))
		case <-time.After(35 * time.Second):
			t.Fatalf("private CLI %v timed out", args)
		}
	}
	policy.prepare(t)
	cli("mount", directory)
	policy.active(t, "after actual mount")
	policy.preserveSentinel(t)
	filePath := filepath.Join(directory, "debrid", "torbox", "Release [123]", "space name.mkv")
	nativeEventually(t, 35*time.Second, "private persistent mount after initiating CLI exits", func() bool { _, err := os.Stat(filePath); return err == nil })
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
	var st *agent.DaemonState
	nativeEventually(t, 35*time.Second, "registered private daemon after mounted read", func() bool {
		st = agent.ReadState()
		return st != nil && st.Status == "running" && agent.IsProcessAlive(st.PID)
	})
	oldPID := st.PID
	cli("daemon", "restart")
	nativeEventually(t, 35*time.Second, "private daemon fresh PID", func() bool {
		st := agent.ReadState()
		return st != nil && st.PID != oldPID && st.Status == "running" && agent.IsProcessAlive(st.PID)
	})
	nativeEventually(t, 35*time.Second, "private persistent remount after restart", func() bool { _, err := os.Stat(filePath); return err == nil })
	read()
	policy.active(t, "after actual restart and mounted read")
	st = agent.ReadState()
	if st == nil {
		t.Fatal("private daemon state disappeared before renewal")
	}
	nativeRenewPersistentIdentity(t, f, filePath, st.PID)
	policy.active(t, "after actual K1-to-K2 renewal")
	for _, disableIntent := range []bool{true, false} {
		if !disableIntent {
			// The first umount saved disabled intent. Declare a new opt-in in
			// this synthetic config; this does not exercise interactive consent.
			loaded, err := config.Load(config.FilePath())
			if err != nil || loaded.Mount.Enabled {
				t.Fatalf("first umount must save disabled intent: %v", err)
			}
			loaded.Mount.Enabled = true
			if err := config.Save(loaded, config.FilePath()); err != nil {
				t.Fatal(err)
			}
			t.Log("synthetic config explicitly opts into second cycle; interactive consent SKIP")
			cli("mount", directory)
			nativeEventually(t, 35*time.Second, "second private persistent mount", func() bool { _, err := os.Stat(filePath); return err == nil })
			read()
		}
		st = agent.ReadState()
		if st == nil || !agent.IsProcessAlive(st.PID) {
			t.Fatal("private daemon not live before umount")
		}
		daemonPID := st.PID
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		out, err := exec.CommandContext(ctx, "pgrep", "-P", strconv.Itoa(daemonPID), "rclone").Output()
		cancel()
		pids := strings.Fields(string(out))
		if err != nil || len(pids) != 1 {
			t.Fatalf("owned persistent rclone before umount: %v %q", err, out)
		}
		rclonePID, err := strconv.Atoi(pids[0])
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := config.Load(config.FilePath())
		if err != nil || !loaded.Mount.Enabled {
			t.Fatalf("private mount intent missing before umount: %v", err)
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
		nativeEventually(t, 15*time.Second, "private mount/DAV/rclone removed", func() bool {
			if _, err := validateMountPoint(directory); err != nil || agent.IsProcessAlive(rclonePID) {
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
			t.Fatalf("private umount persistence failed: %v", err)
		}
		nativeEventually(t, 35*time.Second, "healthy ordinary private daemon after umount", func() bool {
			st = agent.ReadState()
			return st != nil && st.Status == "running" && agent.IsProcessAlive(st.PID)
		})
		policy.active(t, "after actual umount disabled intent="+strconv.FormatBool(disableIntent))
		t.Logf("actual private umount disabled intent=%t: destination reusable, DAV closed, rclone PID=%d gone, healthy daemon PID=%d (before=%d; explicit umount may restart)", disableIntent, rclonePID, st.PID, daemonPID)
	}
	t.Logf("actual private Linux persistence passed; final daemon PID=%d", st.PID)
}

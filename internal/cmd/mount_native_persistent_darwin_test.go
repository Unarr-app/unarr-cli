//go:build darwin

package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/engine"
)

func TestMountNativeMacPersistentPeerRenewedIdentity(t *testing.T) {
	nativeMountOptIn(t, false)
	g := newNativeMacPersistent(t)
	initial := g.api.cfg.Auth.APIKey
	c, err := config.Load(config.FilePath())
	if err != nil {
		t.Fatal(err)
	}
	c.Auth.APIKey = "native-fixture-renewed-device-key"
	g.api.key.Store(c.Auth.APIKey)
	if err := config.Save(c, config.FilePath()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := startRemoteLibrary(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	conn, _ := g.peer(t)
	conn.Close()
	if g.api.cfg.Auth.APIKey != initial || initial == c.Auth.APIKey {
		t.Fatal("regression must retain original fixture K1 while saved config/DAV use K2")
	}
	t.Log("wire peer authenticated saved K2 while original fixture cfg remains K1; kernel/service not started")
}

func TestMountNativeMacPersistentPreparation(t *testing.T) {
	nativeMountOptIn(t, false)
	g := newNativeMacPersistent(t)
	nativeMacDefinition(t, g)
	if _, loaded, err := g.agent.status(); err != nil || loaded {
		t.Fatalf("preparation registered a real label: %t %v", loaded, err)
	}
	t.Log("PREPARATION_ONLY=PASS; no bootstrap/load; not mounted persistence")
}

func TestMountNativeMacPersistent(t *testing.T) {
	nativeMountOptIn(t, false)
	g := newNativeMacPersistent(t)
	nativeMacDefinition(t, g)
	if os.Getenv("UNARR_NATIVE_KERNEL") != "1" || os.Getenv("UNARR_NATIVE_MAC_SERVICE") != "1" {
		t.Skip("prepared private service only; mounted persistence requires both UNARR_NATIVE_MAC_SERVICE=1 and UNARR_NATIVE_KERNEL=1; no bootstrap/load")
	}
	out, err := nativeMacCommand("/usr/bin/kmutil", "showloaded", "--list-only")
	if err != nil || !strings.Contains(string(out), "io.macfuse.filesystems.macfuse") {
		t.Skipf("macFUSE approval/already-loaded extension required; no activation attempted: %v", err)
	}
	// The prepared plist uses fixture-provided HOME; public daemon install is
	// deliberately absent. Actual CLI mount sees an existing service definition.
	g.launched = true
	g.cliRun(t, "mount", g.directory)
	t.Log("initiating mount CLI exited; checking persistent listing/bytes independently")
	g.read(t)
	daemon, child := g.mountedIdentities(t)
	peer, reader := g.peer(t)
	defer peer.Close()
	g.cliRun(t, "daemon", "restart")
	nativeEventually(t, 15*time.Second, "old Mac daemon/rclone identities gone", func() bool { return daemon.gone() && child.gone() })
	_ = peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := reader.ReadByte(); err != io.EOF {
		t.Fatalf("old DAV peer did not drain before new read: %v", err)
	}
	g.read(t)
	fresh, _ := g.mountedIdentities(t)
	if fresh.pid == daemon.pid {
		t.Fatal("actual restart retained old daemon PID")
	}
	t.Logf("ready actual restart drained old daemon%d/rclone%d/DAV before new mounted bytes", daemon.pid, child.pid)
	g.renew(t, fresh.pid)
	for _, savedFalse := range []bool{true, false} {
		if !savedFalse {
			c, err := config.Load(config.FilePath())
			if err != nil || c.Mount.Enabled {
				t.Fatalf("first umount did not save disabled: %v", err)
			}
			c.Mount.Enabled = true
			if err := config.Save(c, config.FilePath()); err != nil {
				t.Fatal(err)
			}
			t.Log("synthetic second-cycle consent explicitly saved; interactive consent coverage SKIP")
			g.cliRun(t, "mount", g.directory)
			g.read(t)
		}
		_, child := g.mountedIdentities(t)
		c, err := config.Load(config.FilePath())
		if err != nil || !c.Mount.Enabled {
			t.Fatalf("mount not enabled before umount: %v", err)
		}
		if savedFalse {
			c.Mount.Enabled = false
			if err := config.Save(c, config.FilePath()); err != nil {
				t.Fatal(err)
			}
		}
		g.cliRun(t, "umount")
		nativeEventually(t, 15*time.Second, "umount DAV/child/destination drained", func() bool {
			if _, err := validateMountPoint(g.directory); err != nil || !child.gone() {
				return false
			}
			conn, err := net.DialTimeout("tcp", c.Mount.Listen, 100*time.Millisecond)
			if err == nil {
				conn.Close()
				return false
			}
			return true
		})
		c, err = config.Load(config.FilePath())
		if err != nil || c.Mount.Enabled {
			t.Fatalf("umount enabled state survived: %v", err)
		}
		nativeEventually(t, 35*time.Second, "healthy ordinary Mac daemon after umount", func() bool {
			st := agent.ReadState()
			if st == nil || st.Status != "running" || !agent.IsProcessAlive(st.PID) {
				return false
			}
			out, loaded, err := g.agent.status()
			return err == nil && loaded && launchdPID(out) == st.PID
		})
		if err := g.owned(); err != nil {
			t.Fatal(err)
		}
		st := agent.ReadState()
		id, err := nativeMacProcess(st.PID, g.cli)
		if err != nil {
			t.Fatal(err)
		}
		g.identities = append(g.identities, id)
		t.Logf("actual umount saved-false=%t: destination reusable/DAV closed/old child gone/healthy daemon%d", savedFalse, st.PID)
	}
	c, err := config.Load(config.FilePath())
	if err != nil || c.Mount.Enabled {
		t.Fatalf("final umount did not disable: %v", err)
	}
	c.Mount.Enabled = true
	if err := config.Save(c, config.FilePath()); err != nil {
		t.Fatal(err)
	}
	t.Log("synthetic consent saved for final live-mounted uninstall; interactive consent coverage SKIP")
	g.cliRun(t, "mount", g.directory)
	g.read(t)
	g.mountedIdentities(t)
	g.uninstallPeer, g.uninstallReader = g.peer(t)
	g.uninstallMounted = true
	t.Log("mounted controls complete; registered cleanup will exercise actual uninstall from verified live mounted daemon/rclone/DAV")
	// Registered cleanup performs actual CLI uninstall and verifies all owned
	// jobs/identities/listeners/destination before removing the private HOME.
}

func nativeMacDefinition(t *testing.T, g *nativeMacPersistent) {
	t.Helper()
	out, err := nativeMacCommand("/usr/bin/plutil", "-extract", "EnvironmentVariables", "json", "-o", "-", g.plist)
	var env map[string]string
	if err != nil || json.Unmarshal(out, &env) != nil {
		t.Fatalf("generated plist environment: %v %s", err, out)
	}
	if len(env) != len(g.env) {
		t.Fatal("unexpected generated service environment")
	}
	for _, entry := range g.env {
		k, v, _ := strings.Cut(entry, "=")
		got, ok := env[k]
		if !ok || got != v {
			t.Fatalf("generated service env mismatch %s", k)
		}
	}
	out, err = nativeMacCommand("/usr/bin/plutil", "-extract", "ProgramArguments", "json", "-o", "-", g.plist)
	var argv []string
	want := []string{g.cli, "start", "--log-file", filepath.Join(config.DataDir(), "unarr.log")}
	if g.sandboxed {
		want = append([]string{"/usr/bin/sandbox-exec", "-f", g.sandbox}, want...)
	}
	if err != nil || json.Unmarshal(out, &argv) != nil || !reflect.DeepEqual(argv, want) {
		t.Fatalf("generated actual CLI argv mismatch: %v %s", err, out)
	}
	if err := g.owned(); err != nil {
		t.Fatal(err)
	}
	t.Log("parsed prepared definition/env/actual CLI argv PASS; all default config/state/log/tools paths confined")
}

func (g *nativeMacPersistent) file() string {
	return filepath.Join(g.directory, "debrid", "torbox", "Release [123]", "space name.mkv")
}

func (g *nativeMacPersistent) read(t *testing.T) {
	t.Helper()
	done := make(chan error, 1)
	g.pending = append(g.pending, done)
	go func() {
		defer close(done)
		want := map[string]int64{}
		for name, b := range g.api.files {
			want["debrid/torbox/"+name] = int64(len(b))
		}
		var last error
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
			got := map[string]int64{}
			err := filepath.WalkDir(g.directory, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					return nil
				}
				info, err := d.Info()
				if err != nil {
					return err
				}
				rel, err := filepath.Rel(g.directory, p)
				if err != nil {
					return err
				}
				got[filepath.ToSlash(rel)] = info.Size()
				return nil
			})
			if err == nil && reflect.DeepEqual(got, want) {
				r, err := os.Open(g.file())
				if err == nil {
					err = nativeReadSequence(r, g.api.files["Release [123]/space name.mkv"], 0)
					closeErr := r.Close()
					if err == nil {
						err = closeErr
					}
				}
				if err == nil {
					done <- nil
					return
				}
				last = err
			} else {
				last = fmt.Errorf("listing=%v want=%v: %v", got, want, err)
			}
			time.Sleep(100 * time.Millisecond)
		}
		done <- fmt.Errorf("mounted listing/range deadline: %v", last)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("mounted IO blocked; cleanup will unmount and join owned reader")
	}
}

func (g *nativeMacPersistent) mountedIdentities(t *testing.T) (nativeMacIdentity, nativeMacIdentity) {
	t.Helper()
	var daemon, child nativeMacIdentity
	nativeEventually(t, 20*time.Second, "registered Mac daemon and one owned rclone", func() bool {
		st := agent.ReadState()
		if st == nil || st.Status != "running" {
			return false
		}
		out, loaded, err := g.agent.status()
		if err != nil || !loaded || launchdPID(out) != st.PID {
			return false
		}
		daemon, err = nativeMacProcess(st.PID, g.cli)
		if err != nil {
			return false
		}
		pids, err := nativeMacCommand("/usr/bin/pgrep", "-P", strconv.Itoa(st.PID), "-x", "rclone")
		fields := strings.Fields(string(pids))
		if err != nil || len(fields) != 1 {
			return false
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			return false
		}
		child, err = nativeMacProcess(pid, filepath.Join(config.Dir(), "tools", "rclone-1.75.1-darwin-arm64", "rclone"))
		return err == nil
	})
	if err := g.owned(); err != nil {
		t.Fatal(err)
	}
	g.identities = append(g.identities, daemon, child)
	t.Logf("actual daemon identity PID%d %s; child PID%d %s (ps creation time seconds)", daemon.pid, daemon.receipt, child.pid, child.receipt)
	parentGroup, err := nativeMacCommand("/bin/ps", "-p", strconv.Itoa(daemon.pid), "-o", "pgid=")
	if err != nil {
		t.Fatal(err)
	}
	childGroup, err := nativeMacCommand("/bin/ps", "-p", strconv.Itoa(child.pid), "-o", "pgid=")
	if err != nil || strings.TrimSpace(string(childGroup)) != strconv.Itoa(child.pid) || strings.TrimSpace(string(childGroup)) == strings.TrimSpace(string(parentGroup)) {
		t.Fatalf("launchd mount child not isolated from daemon group: daemon=%s child=%s err=%v", parentGroup, childGroup, err)
	}
	t.Logf("actual launchd daemon PGID%s; owned rclone PGID%s equals child PID and differs from daemon", strings.TrimSpace(string(parentGroup)), strings.TrimSpace(string(childGroup)))
	return daemon, child
}

func (g *nativeMacPersistent) peer(t *testing.T) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", g.api.cfg.Mount.Listen, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	g.peers = append(g.peers, conn)
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	req, err := http.NewRequest("PROPFIND", "http://"+g.api.cfg.Mount.Listen+"/dav/", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The original fixture config deliberately remains K1 during renewal.
	// A new persistent peer must authenticate the currently saved private K2.
	saved, err := config.Load(config.FilePath())
	if err != nil {
		t.Fatal(err)
	}
	user, password, _ := engine.ResolveWebDAVCreds("", "", saved.Auth.APIKey)
	req.SetBasicAuth(user, password)
	req.Header.Set("Depth", "0")
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 207 || resp.Close {
		t.Fatalf("retained DAV peer invalid: %v %+v", err, resp)
	}
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("unexpected DAV peer bytes")
	} else if n, ok := err.(net.Error); !ok || !n.Timeout() {
		t.Fatalf("DAV peer already closed: %v", err)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, reader
}

func (g *nativeMacPersistent) renew(t *testing.T, daemonPID int) {
	t.Helper()
	c, err := config.Load(config.FilePath())
	if err != nil {
		t.Fatal(err)
	}
	oldResolves := g.api.renewedResolves.Load()
	c.Auth.APIKey = "native-fixture-renewed-device-key"
	g.api.key.Store(c.Auth.APIKey)
	g.api.generation.Add(1)
	if err := config.Save(c, config.FilePath()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	g.pending = append(g.pending, done)
	started := time.Now()
	go func() {
		defer close(done)
		var last error
		for deadline := time.Now().Add(22 * time.Second); time.Now().Before(deadline); {
			r, err := os.Open(g.file())
			if err == nil {
				buf := make([]byte, 4096)
				n, e := r.ReadAt(buf, 987654)
				err = e
				closeErr := r.Close()
				if err == nil {
					err = closeErr
				}
				if err == nil && (n != len(buf) || !bytes.Equal(buf, g.api.files["Release [123]/space name.mkv"][987654:991750])) {
					err = fmt.Errorf("renewed exact range mismatch")
				}
			}
			if err == nil && g.api.renewedResolves.Load() > oldResolves {
				done <- nil
				return
			}
			last = err
			time.Sleep(100 * time.Millisecond)
		}
		done <- fmt.Errorf("K1K2 resolution/range deadline: %v", last)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("renewed range blocked; cleanup will unmount and join reader")
	}
	st := agent.ReadState()
	if st == nil || st.PID != daemonPID || !agent.IsProcessAlive(daemonPID) {
		t.Fatal("renewal replaced/stopped daemon")
	}
	t.Logf("actual persistent K1-to-K2 fresh resolve and exact mounted range after%s; daemonPID%d unchanged", time.Since(started), daemonPID)
}

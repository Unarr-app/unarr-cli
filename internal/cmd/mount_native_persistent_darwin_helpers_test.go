//go:build darwin

package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/service"
)

type nativeMacPersistent struct {
	home, realHome, cli, directory, plist string
	definition                            []byte
	definitionInfo                        os.FileInfo
	pending                               []<-chan error
	peers                                 []net.Conn
	uninstallReader                       *bufio.Reader
	uninstallPeer                         net.Conn
	uninstallMounted                      bool
	env                                   []string
	api                                   *nativeMountFixture
	agent                                 *launchdAgent
	launched                              bool
	identities                            []nativeMacIdentity
}

type nativeMacIdentity struct {
	pid     int
	receipt string
}

// Child probe executes under the exact generated service environment, without
// registering any job. It does not claim a launchd child or mount was executed.
func TestNativeMacPersistentEnvironmentHelper(t *testing.T) {
	if os.Getenv("UNARR_NATIVE_MAC_ENV_PROBE") != "1" {
		return
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	id, err := nativeMacProcess(os.Getpid(), bin)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(map[string]string{"home": os.Getenv("HOME"), "config": config.FilePath(), "data": config.DataDir(), "state": agent.StateFilePath(), "lock": config.LockPath(), "identity": id.receipt})
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println(string(b))
}

func newNativeMacPersistent(t *testing.T) *nativeMacPersistent {
	t.Helper()
	realHome := nativeMacRealHome(t)
	nativeMacAbsent(t, realHome)
	// A fixed UID lock prevents two cooperating fixtures claiming one label.
	lockPath := filepath.Join("/private/tmp", "unarr-native-mac-service-"+strconv.Itoa(os.Getuid())+".lock")
	fd, err := syscall.Open(lockPath, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		t.Fatal(err)
	}
	lock := os.NewFile(uintptr(fd), lockPath)
	info, err := lock.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		lock.Close()
		t.Fatal("unsafe fixture lock")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		t.Fatal("another Mac persistent fixture owns this UID")
	}
	t.Cleanup(func() { _ = syscall.Flock(fd, syscall.LOCK_UN); _ = lock.Close() }) // Keep inode stable for future lock users.
	rawHome, err := os.MkdirTemp("", "unarr-native-mac-home-")
	if err != nil {
		t.Fatal(err)
	}
	home, err := filepath.EvalSymlinks(rawHome)
	if err != nil || home == realHome {
		t.Fatalf("private canonical HOME: %v", err)
	}
	for p := home; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("symlink/inaccessible HOME ancestry: %s %v", p, err)
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	nativeMacPrivatePath(t, home, home)
	if info, err := os.Stat(home); err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("private HOME must be owned0700")
	}
	for _, name := range []string{"UNARR_CONFIG_DIR", "UNARR_API_KEY", "UNARR_API_URL", "UNARR_DOWNLOAD_DIR", "UNARR_COUNTRY", "UNARR_TELEMETRY"} {
		t.Setenv(name, "")
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg-config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "xdg-data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "xdg-cache"))
	g := &nativeMacPersistent{home: home, realHome: realHome, cli: nativeCLIPath(t), directory: filepath.Join(home, "mount"), plist: service.PlistPath(home), api: newNativeMountFixture(t)}
	a, err := newLaunchdAgent(home)
	if err != nil {
		t.Fatal(err)
	}
	g.agent = a
	t.Cleanup(func() { g.cleanup(t) })
	g.env = []string{"HOME=" + home, "PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin", "XDG_CONFIG_HOME=" + os.Getenv("XDG_CONFIG_HOME"), "XDG_DATA_HOME=" + os.Getenv("XDG_DATA_HOME"), "XDG_CACHE_HOME=" + os.Getenv("XDG_CACHE_HOME"), "TMPDIR=" + home + "/tmp", "UNARR_NO_TELEMETRY=1", "NO_COLOR=1", "UNARR_CONFIG_DIR=", "UNARR_API_KEY=", "UNARR_API_URL=", "UNARR_DOWNLOAD_DIR=", "UNARR_COUNTRY=", "UNARR_TELEMETRY="}
	for _, p := range []string{g.directory, config.Dir(), home + "/tmp", home + "/downloads", home + "/catalog", home + "/nzbs"} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
		nativeMacPrivatePath(t, home, p)
	}
	f := g.api
	f.cfg.Agent.ID, f.cfg.Agent.Name = "native-mac-fixture", "Native Mac Fixture"
	f.cfg.Download.Dir = home + "/downloads"
	f.cfg.Download.PreferredMethods = []string{"debrid"}
	f.cfg.Download.HTTPSStreamPort, f.cfg.Download.StreamPort = 0, 0
	f.cfg.Download.AutoHTTPSUpnp, f.cfg.Download.EnableUPnP = false, false
	f.cfg.Download.Transcode.Enabled = false
	f.cfg.Download.Funnel.Enabled, f.cfg.Download.VPN.Enabled = false, false
	f.cfg.Daemon.Downlink = "poll"
	f.cfg.Mount.CacheDir, f.cfg.Mount.NZBDir = home+"/catalog", home+"/nzbs"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.cfg.Mount.Listen = ln.Addr().String()
	_ = ln.Close()
	if err := config.Save(f.cfg, config.FilePath()); err != nil {
		t.Fatal(err)
	}
	rclone := os.Getenv("UNARR_NATIVE_RCLONE")
	if !filepath.IsAbs(rclone) || filepath.Base(rclone) != "rclone" || filepath.Base(filepath.Dir(rclone)) != "rclone-1.75.1-darwin-arm64" {
		t.Fatal("verified task-local official Darwin rclone required")
	}
	data, err := os.ReadFile(rclone)
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(config.Dir(), "tools", filepath.Base(filepath.Dir(rclone)))
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "rclone"), data, 0700); err != nil {
		t.Fatal(err)
	}
	envXML := "<key>EnvironmentVariables</key><dict>"
	for _, item := range g.env {
		k, v, _ := strings.Cut(item, "=")
		var escaped bytes.Buffer
		_ = xml.EscapeText(&escaped, []byte(v))
		envXML += "<key>" + k + "</key><string>" + escaped.String() + "</string>"
	}
	envXML += "</dict>\n"
	// Fixture service env, deliberately different from the production installer.
	tmpl := strings.Replace(launchdTemplate, "  <key>RunAtLoad</key>", envXML+"  <key>RunAtLoad</key>", 1)
	if err := writeServiceFile(g.plist, tmpl, serviceData{BinPath: g.cli, Home: home, LogDir: config.DataDir()}); err != nil {
		t.Fatal(err)
	}
	g.definition, err = os.ReadFile(g.plist)
	if err != nil {
		t.Fatal(err)
	}
	g.definitionInfo, err = os.Lstat(g.plist)
	if err != nil {
		t.Fatal(err)
	}
	nativeMacPrivatePath(t, home, g.plist)
	if out, err := nativeMacCommand("/usr/bin/plutil", "-lint", g.plist); err != nil {
		t.Fatalf("private plist invalid: %v %s", err, out)
	}
	for _, p := range []string{config.FilePath(), config.DataDir(), agent.StateFilePath(), config.LockPath(), cache, daemonBootLogPath()} {
		nativeMacPrivatePath(t, home, p)
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	probe := exec.CommandContext(ctx, bin, "-test.run", "^TestNativeMacPersistentEnvironmentHelper$", "-test.v")
	probe.Env = append(append([]string{}, g.env...), "UNARR_NATIVE_MAC_ENV_PROBE=1")
	out, err := probe.CombinedOutput()
	if err != nil {
		t.Fatalf("private environment probe: %v %s", err, out)
	}
	var paths map[string]string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "{") {
			err = json.Unmarshal([]byte(line), &paths)
			break
		}
	}
	if err != nil || paths["home"] != home || paths["config"] != config.FilePath() || paths["data"] != config.DataDir() || paths["state"] != agent.StateFilePath() || paths["lock"] != config.LockPath() || paths["identity"] == "" {
		t.Fatalf("private child path mismatch: %v %s", err, out)
	}
	cmd := exec.CommandContext(ctx, g.cli, "version")
	cmd.Env = g.env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("native isolated CLI version: %v %s", err, out)
	}
	t.Logf("prepared fixture HOME=%s config=%s state=%s tools=%s; actual child environment probe PASS; no bootstrap/load/label mutation", home, config.FilePath(), agent.StateFilePath(), cache)
	return g
}

func (g *nativeMacPersistent) cliRun(t *testing.T, args ...string) {
	t.Helper()
	if err := g.owned(); err != nil {
		t.Fatal(err)
	}
	p := &nativeCLIProcess{cmd: exec.Command(g.cli, args...), done: make(chan struct{})}
	p.cmd.Env = g.env
	p.cmd.Dir = g.home
	p.cmd.Stdout = &p.output
	p.cmd.Stderr = &p.output
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.err = p.cmd.Wait(); close(p.done) }()
	t.Cleanup(func() { p.stop(t) })
	select {
	case <-p.done:
		if p.err != nil {
			t.Fatalf("actual private CLI %v: %v %s", args, p.err, p.output.String())
		}
		t.Logf("CLI %v: %s", args, strings.TrimSpace(p.output.String()))
	case <-time.After(40 * time.Second):
		t.Fatalf("private CLI %v timed out", args)
	}
	if len(args) == 2 && args[0] == "daemon" && args[1] == "uninstall" {
		return
	}
	if err := g.owned(); err != nil {
		t.Fatal(err)
	}
}

func (g *nativeMacPersistent) cleanup(t *testing.T) {
	t.Helper()
	defer func() {
		for _, conn := range g.peers {
			_ = conn.Close()
		}
	}()
	if len(g.definition) == 0 {
		return
	} // No launch could occur before preparation finished.
	if err := g.owned(); err != nil {
		t.Errorf("refuse foreign/replaced fixture cleanup: %v", err)
		return
	}
	if g.launched {
		if err := g.cleanupIdentities(); err != nil {
			t.Errorf("unsafe cleanup identity: %v", err)
			return
		}
		g.cliRun(t, "daemon", "uninstall")
		if g.uninstallMounted {
			_ = g.uninstallPeer.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := g.uninstallReader.ReadByte(); err != io.EOF {
				t.Errorf("live mounted uninstall did not drain old DAV peer: %v", err)
				return
			}
		}
	} else if err := os.Remove(g.plist); err != nil {
		t.Error(err)
	}
	for _, id := range g.identities {
		if !id.gone() {
			t.Errorf("owned process survived cleanup: PID%d %s", id.pid, id.receipt)
			return
		}
	}
	for _, done := range g.pending {
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			t.Error("mounted IO worker survived cleanup")
			return
		}
	}
	if _, err := os.Lstat(g.plist); !os.IsNotExist(err) {
		t.Errorf("private plist survived cleanup: %v", err)
		return
	}
	if _, err := validateMountPoint(g.directory); err != nil {
		t.Errorf("destination not reusable: %v", err)
		return
	}
	if conn, err := net.DialTimeout("tcp", g.api.cfg.Mount.Listen, 100*time.Millisecond); err == nil {
		conn.Close()
		t.Error("DAV listener survived cleanup")
		return
	}
	nativeMacAbsent(t, g.realHome)
	if err := os.RemoveAll(g.home); err != nil {
		t.Error(err)
		return
	}
	t.Log("private HOME/plist/labels/processes/DAV/mount cleaned; real-home guard PASS")
	if g.uninstallMounted {
		t.Log("MOUNTED_PERSISTENCE=PASS; live mounted actual CLI uninstall drained verified daemon/rclone/DAV; fixture-provided service env, not installer HOME propagation")
	}

}

func (g *nativeMacPersistent) cleanupIdentities() error {
	out, loaded, err := g.agent.status()
	if err != nil {
		return err
	}
	if !loaded || launchdPID(out) == 0 {
		return nil
	}
	pid := launchdPID(out)
	id, err := nativeMacProcess(pid, g.cli)
	if err != nil {
		return err
	}
	g.identities = append(g.identities, id)
	children, err := nativeMacCommand("/usr/bin/pgrep", "-P", strconv.Itoa(pid), "-x", "rclone")
	if e, ok := err.(*exec.ExitError); ok && e.ExitCode() == 1 && len(bytes.TrimSpace(children)) == 0 {
		return nil
	}
	if err != nil {
		return err
	}
	for _, raw := range strings.Fields(string(children)) {
		pid, err := strconv.Atoi(raw)
		if err != nil {
			return err
		}
		id, err := nativeMacProcess(pid, filepath.Join(config.Dir(), "tools", "rclone-1.75.1-darwin-arm64", "rclone"))
		if err != nil {
			return err
		}
		g.identities = append(g.identities, id)
	}
	return nil
}

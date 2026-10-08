package cmd

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/engine"
	"github.com/Unarr-app/unarr-cli/internal/winproc"
)

type nativeCLIProcess struct {
	cmd    *exec.Cmd
	output bytes.Buffer // Read only after Wait completes.
	done   chan struct{}
	err    error
	once   sync.Once
}

func nativeCLIPath(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"UNARR_API_KEY", "UNARR_API_URL", "UNARR_DOWNLOAD_DIR", "UNARR_CONFIG_DIR"} {
		if os.Getenv(name) != "" {
			t.Fatalf("clear ambient %s before synthetic native subprocesses", name)
		}
	}
	binary := os.Getenv("UNARR_NATIVE_CLI")
	if !filepath.IsAbs(binary) {
		t.Fatal("UNARR_NATIVE_CLI must identify the exact isolated CLI artifact")
	}
	if _, err := os.Stat(binary); err != nil {
		t.Fatal(err)
	}
	return binary
}

func startNativeCLI(t *testing.T, args ...string) *nativeCLIProcess {
	t.Helper()
	p := &nativeCLIProcess{cmd: exec.Command(nativeCLIPath(t), args...), done: make(chan struct{})}
	winproc.HideWindow(p.cmd)
	p.cmd.Dir = filepath.Dir(p.cmd.Path) // A Windows UNC cwd breaks child commands.
	p.cmd.Env = append(os.Environ(), "UNARR_NO_TELEMETRY=1", "NO_COLOR=1")
	p.cmd.Stdout, p.cmd.Stderr = &p.output, &p.output
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.err = p.cmd.Wait(); close(p.done) }()
	t.Cleanup(func() { p.stop(t) })
	return p
}

func (p *nativeCLIProcess) stop(t *testing.T) {
	t.Helper()
	p.once.Do(func() {
		select {
		case <-p.done:
			return
		default:
		}
		_ = p.cmd.Process.Kill()
		select {
		case <-p.done:
		case <-time.After(7 * time.Second):
			t.Error("owned CLI process survived cleanup")
		}
	})
}

func nativeCLIConfig(t *testing.T, f *nativeMountFixture) (string, *remoteLibrary) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.cfg.Mount.Listen = ln.Addr().String()
	_ = ln.Close()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := config.Save(f.cfg, path); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil || len(loaded.Auth.Mirrors) != 1 || loaded.Auth.Mirrors[0] != f.web.URL {
		t.Fatalf("fixture mirror isolation lost: %v", err)
	}
	user, password, _ := engine.ResolveWebDAVCreds("", "", f.cfg.Auth.APIKey)
	return path, &remoteLibrary{URL: "http://" + f.cfg.Mount.Listen, user: user, password: password}
}

func TestMountNativeCLIAccess(t *testing.T) {
	nativeMountOptIn(t, false)
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"free", 403}, {"trial", 403}, {"expired-subscription", 403}, {"expired-key", 401}, {"revoked", 410},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNativeMountFixture(t)
			f.access.Store(int32(tc.status))
			path, _ := nativeCLIConfig(t, f)
			p := startNativeCLI(t, "mount", "serve", "--config", path)
			select {
			case <-p.done:
				if p.err == nil {
					t.Fatal("denied identity started mount serve")
				}
				t.Logf("synthetic %s denial: %s", tc.name, p.output.String())
			case <-time.After(8 * time.Second):
				t.Fatal("denied CLI did not terminate")
			}
			for _, dir := range []string{f.cfg.Mount.CacheDir, f.cfg.Mount.NZBDir} {
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Fatalf("denied identity touched fixture directory: %s (%v)", dir, err)
				}
			}
		})
	}
	// These response fixtures establish client rejection, not website billing policy.
}

func TestMountNativeCLIServeLifecycle(t *testing.T) {
	nativeMountOptIn(t, false)
	f := newNativeMountFixture(t)
	path, s := nativeCLIConfig(t, f)
	p := startNativeCLI(t, "mount", "serve", "--config", path)
	read := func() bool {
		r, err := nativeDAVRequest(context.Background(), s, "GET", "debrid/torbox/Release [123]/space name.mkv", "bytes=1000000-1004095")
		if err != nil {
			return false
		}
		b, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		return err == nil && r.StatusCode == http.StatusPartialContent && bytes.Equal(b, f.files["Release [123]/space name.mkv"][1000000:1004096])
	}
	nativeEventually(t, 8*time.Second, "real CLI custom-config DAV range", read)
	f.generation.Add(1)
	if !read() {
		t.Fatal("real CLI signed URL renewal failed")
	}
	f.cdnOutage.Store(1)
	if read() {
		t.Fatal("real CLI succeeded during CDN outage")
	}
	f.cdnOutage.Store(0)
	nativeEventually(t, 6*time.Second, "real CLI read after short network loss", read)
	f.access.Store(http.StatusGone)
	select {
	case <-p.done:
		if p.err == nil {
			t.Error("revoked CLI exited successfully")
		}
		t.Logf("real CLI entitlement watcher stopped PID %d: %s", p.cmd.Process.Pid, p.output.String())
	case <-time.After(36 * time.Second):
		t.Fatal("real CLI 30s entitlement watcher did not terminate revoked session")
	}
	if r, err := nativeDAVRequest(context.Background(), s, "PROPFIND", "", ""); err == nil {
		_ = r.Body.Close()
		t.Fatal("revoked CLI retained DAV listener")
	}
	f.access.Store(0)
	// A new allowed identity must establish its own session rather than reuse
	// a revoked credential cached by the previous subprocess/catalog.
	f.key.Store("native-fixture-replacement-key")
	fresh := f.cfg
	fresh.Auth.APIKey = f.key.Load().(string)
	if err := config.Save(fresh, path); err != nil {
		t.Fatal(err)
	}
	s.user, s.password, _ = engine.ResolveWebDAVCreds("", "", fresh.Auth.APIKey)
	p = startNativeCLI(t, "mount", "serve", "--config", path)
	nativeEventually(t, 8*time.Second, "fresh allowed CLI reuses catalog/listener", read)
	p.stop(t)
	if r, err := nativeDAVRequest(context.Background(), s, "PROPFIND", "", ""); err == nil {
		_ = r.Body.Close()
		t.Fatal("stopped CLI retained listener")
	}
	// mount serve is an actual subprocess but is not persistent service activation.
}

func TestMountNativeWindowsLetterRoundTrip(t *testing.T) {
	nativeMountOptIn(t, false)
	if runtime.GOOS != "windows" {
		t.Skip("native Windows config semantics required")
	}
	cfg := config.Default()
	cfg.Mount.Enabled, cfg.Mount.Directory = true, "X:"
	path := filepath.Join(t.TempDir(), "letter-config.toml")
	if err := config.Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Mount.Directory != "X:" {
		t.Fatalf("letter destination changed: %q", loaded.Mount.Directory)
	}
	if err := loaded.Mount.Validate(); err != nil {
		t.Fatalf("persisted Windows letter rejected: %v", err)
	}
}

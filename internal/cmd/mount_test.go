package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/remotefs"
)

func TestRemoteLibraryDisabledHasNoSideEffects(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "must-not-exist")
	cfg := config.Default()
	cfg.Mount.CacheDir = dir
	if _, err := startRemoteLibrary(context.Background(), cfg); err == nil {
		t.Fatal("disabled mount started")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("disabled mount touched disk", err)
	}
}

func TestRemoteLibraryServeAndShutdown(t *testing.T) {
	cfg := config.Default()
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"accounts":[],"allowed":true}`)
	}))
	defer web.Close()
	cfg.Auth.APIURL, cfg.Auth.APIKey = web.URL, "test-agent-key"
	cfg.Mount = config.MountConfig{Enabled: true, Listen: "127.0.0.1:0", CacheDir: t.TempDir(), NZBDir: t.TempDir()}
	s, err := startRemoteLibrary(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("PROPFIND", s.URL+"/dav/", nil)
	req.SetBasicAuth(s.user, s.password)
	req.Header.Set("Depth", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 207 {
		s.Close()
		t.Fatal(resp.StatusCode)
	}
	s.Close()
	client := &http.Client{Timeout: time.Second}
	resp, err = client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("listener survived shutdown")
	}
}

type mountMemoryReader struct{ *bytes.Reader }

func (mountMemoryReader) Close() error { return nil }

// This opt-in test exercises the actual kernel mount and rclone process, not a
// filesystem mock. Enable only on hosts with FUSE/WinFsp installed.
func TestRcloneKernelMount(t *testing.T) {
	if os.Getenv("UNARR_TEST_RCLONE_MOUNT") != "1" {
		t.Skip("set UNARR_TEST_RCLONE_MOUNT=1 to exercise an actual FUSE mount")
	}
	if runtime.GOOS == "windows" {
		t.Skip("kernel smoke currently uses POSIX mountpoint lifecycle")
	}
	if _, err := exec.LookPath("rclone"); err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("abcdefgh01234567"), 1<<17)
	f := remotefs.New()
	err := f.Replace([]remotefs.Entry{{Path: "rd/Release [123]/a & b.mkv", Key: "file", Size: int64(len(data)), Modified: time.Now(), Open: func(context.Context) (io.ReadSeekCloser, error) { return mountMemoryReader{bytes.NewReader(data)}, nil }}})
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(remotefs.Handler(f, "u", "p"))
	defer httpSrv.Close()
	s := &remoteLibrary{URL: httpSrv.URL, user: "u", password: "p"}
	mountpoint := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runRclone(ctx, s, mountpoint) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("rclone did not unmount")
		}
	}()
	filePath := filepath.Join(mountpoint, "rd", "Release [123]", "a & b.mkv")
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(filePath); err == nil {
			break
		}
		select {
		case err := <-done:
			done <- nil
			t.Fatalf("rclone exited before mount: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("mount did not become readable")
		}
		time.Sleep(30 * time.Millisecond)
	}
	r, err := os.Open(filePath)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	for _, offset := range []int64{0, 1000000, int64(len(data) - len(buf)), 128000} {
		n, err := r.ReadAt(buf, offset)
		if err != nil || n != len(buf) || !bytes.Equal(buf, data[offset:offset+int64(len(buf))]) {
			_ = r.Close()
			t.Fatalf("bad mounted read at %d: %d %v", offset, n, err)
		}
	}
	_ = r.Close()
	if err := os.WriteFile(filepath.Join(mountpoint, "illegal"), []byte("x"), 0o600); err == nil {
		t.Fatal("mount is writable")
	}
}

func TestRcloneEnvironmentDoesNotInheritOverrides(t *testing.T) {
	t.Setenv("RCLONE_CONFIG_UNARR_URL", "http://wrong")
	t.Setenv("RCLONE_WEBDAV_BEARER_TOKEN_COMMAND", "unexpected-helper")
	env := strings.Join(rcloneEnvironment(&remoteLibrary{URL: "http://127.0.0.1:11820", user: "u", password: "raw-secret"}, "obscured"), "\n")
	if strings.Contains(env, "wrong") || strings.Contains(env, "unexpected-helper") || strings.Contains(env, "raw-secret") {
		t.Fatal("unsafe child environment")
	}
	if !strings.Contains(env, "RCLONE_CONFIG_UNARR_PASS=obscured") {
		t.Fatal("password config missing")
	}
}

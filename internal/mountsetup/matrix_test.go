package mountsetup

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The test executable doubles as a native subprocess fixture on every OS.
// No shell-script emulation or executable permission assumptions on Windows.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "mount" {
		fixtureMain()
		return
	}
	os.Exit(m.Run())
}

func fixtureMain() {
	exe, _ := os.Executable()
	mode, _ := os.ReadFile(exe + ".mode")
	switch string(mode) {
	case "args-receipt":
		data, err := json.Marshal(os.Args[1:])
		if err != nil || os.WriteFile(exe+".args", data, 0o600) != nil {
			os.Exit(2)
		}
		fmt.Println("--read-only")
	case "no-mount":
		fmt.Println("Install a build with mount support")
	case "old-incompatible":
		// An older binary can have mount/read-only but lack a backend flag.
		if strings.Contains(strings.Join(os.Args, " "), "--webdav-pacer-min-sleep") {
			os.Exit(2)
		}
		fmt.Println("--read-only")
	case "hang":
		time.Sleep(time.Minute)
	case "failure":
		os.Exit(1)
	default:
		fmt.Println("--read-only")
	}
}

func TestMountFlagsReadOnlyCapabilityProbe(t *testing.T) {
	args := capabilityProbeArguments(t)
	readOnly, security := 0, 0
	for i, arg := range args {
		if arg == "--read-only" {
			readOnly++
		}
		if strings.HasPrefix(arg, "FileSecurity=") {
			security++
			if i == 0 || args[i-1] != "-o" {
				t.Fatal("filesystem security was not passed as one mount option", args)
			}
		}
	}
	if readOnly != 1 {
		t.Fatal("capability probe lost its read-only requirement", args)
	}
	wantSecurity := 0
	if runtime.GOOS == "windows" {
		wantSecurity = 1
	}
	if security != wantSecurity {
		t.Fatalf("filesystem security options=%d want=%d: %v", security, wantSecurity, args)
	}
}

func capabilityProbeArguments(t *testing.T) []string {
	t.Helper()
	probe := nativeFixture(t, t.TempDir(), "args-receipt")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !mountCapable(ctx, probe) {
		t.Fatal("private capability probe did not complete")
	}
	data, err := os.ReadFile(probe + ".args")
	if err != nil {
		t.Fatal("private subprocess did not record actual arguments", err)
	}
	var args []string
	if err := json.Unmarshal(data, &args); err != nil {
		t.Fatal(err)
	}
	if len(args) < 2 || args[0] != "mount" || args[1] != "--help" {
		t.Fatal("receipt was not from the actual capability entry", args)
	}
	return args
}

func nativeFixture(t *testing.T, dir, mode string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	name := "rclone"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dest := filepath.Join(dir, name)
	if err := os.WriteFile(dest, data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest+".mode", []byte(mode), 0o600); err != nil {
		t.Fatal(err)
	}
	return dest
}

func fixtureServer(t *testing.T) (artifact, *http.Client, *atomic.Int32) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	a, err := rcloneArtifact(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	w, err := z.Create(a.Member)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(buf.Bytes())
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write(buf.Bytes())
	}))
	t.Cleanup(s.Close)
	a.URL, a.SHA256 = s.URL, hex.EncodeToString(hash[:])
	return a, s.Client(), &calls
}

func TestRcloneInstallationMatrix(t *testing.T) {
	a, client, calls := fixtureServer(t)
	for _, mode := range []string{"absent", "compatible", "old-compatible", "old-incompatible", "no-mount", "failure", "corrupt-cache"} {
		t.Run(mode, func(t *testing.T) {
			binDir := t.TempDir()
			t.Setenv("PATH", binDir)
			opts := Options{Directory: t.TempDir(), Output: io.Discard}
			before := calls.Load()
			var original string
			if mode != "absent" && mode != "corrupt-cache" {
				original = nativeFixture(t, binDir, mode)
			}
			if mode == "corrupt-cache" {
				dir := filepath.Join(opts.Directory, "rclone-"+rcloneVersion+"-"+runtime.GOOS+"-"+runtime.GOARCH)
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, filepath.Base(a.Member)), []byte("interrupted installation"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			path, err := ensureRcloneArtifact(context.Background(), opts, a, client)
			if err != nil {
				t.Fatal(err)
			}
			reuse := mode == "compatible" || mode == "old-compatible"
			wantDownloads := int32(1)
			if reuse {
				wantDownloads = 0
			}
			if calls.Load()-before != wantDownloads || (path == original) != reuse {
				t.Fatalf("mode=%s path=%s downloads=%d", mode, path, calls.Load()-before)
			}
			again, err := ensureRcloneArtifact(context.Background(), opts, a, client)
			if err != nil || again != path || calls.Load()-before != wantDownloads {
				t.Fatal("second setup was not idempotent", again, err)
			}
			if original != "" {
				got, _ := os.ReadFile(original + ".mode")
				if string(got) != mode {
					t.Fatal("changed the user's existing installation")
				}
			}
		})
	}
}

func TestConcurrentSetupDownloadsOnce(t *testing.T) {
	a, client, calls := fixtureServer(t)
	t.Setenv("PATH", t.TempDir())
	opts := Options{Directory: t.TempDir(), Output: io.Discard}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ensureRcloneArtifact(context.Background(), opts, a, client); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("downloaded %d times", calls.Load())
	}
}

func TestCapabilityProbeCancellation(t *testing.T) {
	path := nativeFixture(t, t.TempDir(), "hang")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if mountCapable(ctx, path) {
		t.Fatal("hung binary accepted")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("probe ignored cancellation")
	}
}

func TestCancelledSetupDoesNotTouchDisk(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := filepath.Join(t.TempDir(), "untouched")
	_, err := ensureRcloneArtifact(ctx, Options{Directory: dir, Output: io.Discard}, artifact{}, http.DefaultClient)
	if err == nil {
		t.Fatal("cancelled setup succeeded")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("cancelled setup touched disk")
	}
}

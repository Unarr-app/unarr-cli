package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

type nativeMountedSession struct {
	s             *remoteLibrary
	directory     string
	cancel        context.CancelFunc
	done          chan error
	pids          []int
	once          sync.Once
	productionURL string
	observation   *nativeDAVObservation
}

func startNativeMountedSession(t *testing.T, f *nativeMountFixture, directory string) *nativeMountedSession {
	t.Helper()
	binary := os.Getenv("UNARR_NATIVE_RCLONE")
	if binary == "" {
		var err error
		binary, err = exec.LookPath("rclone")
		if err != nil {
			t.Fatal(err)
		}
	}
	f.cfg.Mount.Directory = directory
	ctx, cancel := context.WithCancel(context.Background())
	s, err := startRemoteLibrary(ctx, f.cfg)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	s.rclone = binary
	m := &nativeMountedSession{s: s, directory: directory, cancel: cancel, done: make(chan error, 1)}
	m.productionURL = s.URL
	if runtime.GOOS == "windows" && t.Name() == "TestMountNativeKernelIO" && os.Getenv("UNARR_NATIVE_DAV_OBSERVE") == "1" {
		// A separate fixture proxy observes both control and experimental runs;
		// the production Handler is never swapped while Serve is running.
		m.observation = nativeObserveDAV(t, s.URL)
		s.URL = m.observation.server.URL
		m.cancel = func() { cancel(); m.observation.close() }
	}
	go func() { m.done <- runRclone(s.ctx, s, directory) }()
	t.Cleanup(func() { m.stop(t) })
	name := filepath.Join(directory, "debrid", "torbox", "Release [123]", "space name.mkv")
	nativeEventually(t, 15*time.Second, "readable actual kernel mount", func() bool {
		select {
		case err := <-m.done:
			m.done <- err
			t.Fatalf("rclone exited before mounted I/O: %v", err)
		default:
		}
		_, err := os.Stat(name)
		return err == nil
	})
	m.pids = nativeRcloneChildren(t)
	if len(m.pids) != 1 {
		t.Fatalf("want one owned rclone child, got %v", m.pids)
	}
	t.Logf("native mount directory=%q rclone PID=%d", directory, m.pids[0])
	return m
}

func nativeRcloneChildren(t *testing.T) []int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		q := fmt.Sprintf("Get-CimInstance Win32_Process -Filter 'ParentProcessId = %d' | Where-Object {$_.Name -like 'rclone*'} | ForEach-Object {$_.ProcessId}", os.Getpid())
		cmd = exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", q)
	} else {
		cmd = exec.CommandContext(ctx, "pgrep", "-P", strconv.Itoa(os.Getpid()), "rclone")
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("query own rclone children: %v", err)
	}
	var pids []int
	for _, line := range strings.Fields(string(out)) {
		p, err := strconv.Atoi(line)
		if err == nil {
			pids = append(pids, p)
		}
	}
	return pids
}

func (m *nativeMountedSession) stop(t *testing.T) {
	t.Helper()
	m.once.Do(func() {
		m.cancel()
		select {
		case err := <-m.done:
			if err != nil {
				t.Errorf("mount exit: %v", err)
			}
		case <-time.After(8 * time.Second):
			t.Error("rclone cleanup exceeded 8s")
		}
		closed := make(chan struct{})
		go func() { m.s.Close(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(7 * time.Second):
			t.Error("DAV/catalog cleanup exceeded 7s")
		}
		for _, pid := range m.pids {
			if agent.IsProcessAlive(pid) {
				t.Errorf("owned rclone PID %d survived cleanup", pid)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if resp, err := nativeDAVRequest(ctx, m.s, "PROPFIND", "", ""); err == nil {
			_ = resp.Body.Close()
			t.Error("DAV listener survived cleanup")
		}
		address := strings.TrimPrefix(m.productionURL, "http://")
		if conn, err := net.DialTimeout("tcp", address, time.Second); err == nil {
			_ = conn.Close()
			t.Error("production DAV listener survived cleanup")
		}
		if _, err := validateMountPoint(m.directory); err != nil {
			t.Errorf("destination not reusable after cleanup: %v", err)
		}
	})
}

func nativeTestDestination(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		return filepath.Join(dir, "mount")
	}
	return dir
}

func TestMountNativeKernelIO(t *testing.T) {
	nativeMountOptIn(t, true)
	f := newNativeMountFixture(t)
	m := startNativeMountedSession(t, f, nativeTestDestination(t))
	want := make(map[string]int64)
	for name, data := range f.files {
		want["debrid/torbox/"+name] = int64(len(data))
	}
	if got := nativeMountedFiles(t, m.directory); !reflect.DeepEqual(got, want) {
		t.Fatalf("mounted listing/sizes got=%v want=%v", got, want)
	}
	if f.media.Load() != 0 || f.resolves.Load() != 0 {
		t.Fatal("directory/stat operations fetched media")
	}
	for name, data := range f.files {
		if strings.HasSuffix(name, "/blocked.mkv") {
			continue
		}
		file, err := os.Open(filepath.Join(m.directory, "debrid", "torbox", filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if err := nativeReadSequence(file, data, 0); err != nil {
			_ = file.Close()
			t.Fatalf("%q: %v", name, err)
		}
		_ = file.Close()
	}
	t.Run("independent-handles", func(t *testing.T) {
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for worker := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				name := "Release [123]/space name.mkv"
				r, err := os.Open(filepath.Join(m.directory, "debrid", "torbox", filepath.FromSlash(name)))
				if err != nil {
					errs <- err
					return
				}
				defer r.Close()
				errs <- nativeReadSequence(r, f.files[name], worker*997)
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Error(err)
			}
		}
	})
	nativeAssertWriteDenial(t, m.directory, f.files)
	if m.observation != nil {
		m.observation.assertMutationReplies(t)
	}
	if got := nativeMountedFiles(t, m.directory); !reflect.DeepEqual(got, want) {
		t.Fatal("mutations changed mounted listing or sizes")
	}
	m.stop(t)
	// A second real mount of the same destination proves kernel cleanup/reuse.
	m = startNativeMountedSession(t, f, m.directory)
	if got := nativeMountedFiles(t, m.directory); !reflect.DeepEqual(got, want) {
		t.Fatal("remount changed listing")
	}
	m.stop(t)
}

// Opt-in observation is common to the permission probe and its unmodified
// control. It records methods/statuses, never auth headers or media bytes.
type nativeDAVObservation struct {
	server  *httptest.Server
	mu      sync.Mutex
	methods map[string]int
	replies map[string]int
	once    sync.Once
}

func nativeObserveDAV(t *testing.T, destination string) *nativeDAVObservation {
	t.Helper()
	target, err := url.Parse(destination)
	if err != nil {
		t.Fatal(err)
	}
	o := &nativeDAVObservation{methods: make(map[string]int), replies: make(map[string]int)}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(r *http.Response) error {
		o.mu.Lock()
		o.replies[fmt.Sprintf("%s:%d", r.Request.Method, r.StatusCode)]++
		o.mu.Unlock()
		return nil
	}
	o.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		o.methods[r.Method]++
		o.mu.Unlock()
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(o.close)
	return o
}

func (o *nativeDAVObservation) close() {
	o.once.Do(func() { o.server.CloseClientConnections(); o.server.Close() })
}

func (o *nativeDAVObservation) assertMutationReplies(t *testing.T) {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	t.Logf("fixture DAV observation methods=%v replies=%v; same route in permission control and experiment", o.methods, o.replies)
	for _, method := range []string{"PUT", "DELETE", "MOVE", "MKCOL", "PATCH"} {
		for reply, count := range o.replies {
			if strings.HasPrefix(reply, method+":") {
				status, err := strconv.Atoi(strings.TrimPrefix(reply, method+":"))
				if err != nil || status < 400 {
					t.Errorf("DAV mutation accepted: %s count=%d", reply, count)
				}
			}
		}
	}
}

func nativeMountedFiles(t *testing.T, directory string) map[string]int64 {
	t.Helper()
	nativeRootDiagnostics(t, directory)
	got := make(map[string]int64)
	// WinFsp directory mounts are reparse roots. Stat the selected filesystem
	// root through DirFS while keeping WalkDir's no-follow policy inside it.
	err := fs.WalkDir(os.DirFS(directory), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("unexpected mounted symlink %q", p)
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		got[p] = info.Size()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func nativeReadSequence(r *os.File, data []byte, shift int) error {
	buf := make([]byte, 4096)
	for _, off := range []int64{int64(shift), 1000000 + int64(shift), int64(len(data) - len(buf)), 128000 + int64(shift)} {
		if _, err := r.Seek(off, io.SeekStart); err != nil {
			return err
		}
		n, err := io.ReadFull(r, buf)
		if err != nil || n != len(buf) || !bytes.Equal(buf, data[off:off+int64(len(buf))]) {
			return fmt.Errorf("seek/read at %d: n=%d err=%v", off, n, err)
		}
	}
	if _, err := r.Seek(-17, io.SeekEnd); err != nil {
		return err
	}
	n, err := io.ReadFull(r, buf[:32])
	if n != 17 || err != io.ErrUnexpectedEOF || !bytes.Equal(buf[:17], data[len(data)-17:]) {
		return fmt.Errorf("tail/EOF n=%d err=%v", n, err)
	}
	n, err = r.Read(buf)
	if n != 0 || err != io.EOF {
		return fmt.Errorf("EOF n=%d err=%v", n, err)
	}
	return nil
}

func nativeAssertWriteDenial(t *testing.T, dir string, files map[string][]byte) {
	t.Helper()
	p := filepath.Join(dir, "debrid", "torbox", "Release [123]", "space name.mkv")
	checks := map[string]func() error{
		"create":   func() error { return os.WriteFile(filepath.Join(dir, "illegal"), []byte("x"), 0600) },
		"truncate": func() error { return os.Truncate(p, 1) },
		"overwrite": func() error {
			r, e := os.OpenFile(p, os.O_WRONLY, 0)
			if e != nil {
				return e
			}
			defer r.Close()
			_, e = r.WriteAt([]byte("x"), 0)
			return e
		},
		"rename": func() error { return os.Rename(p, p+".renamed") },
		"delete": func() error { return os.Remove(p) },
		"mkdir":  func() error { return os.Mkdir(filepath.Join(dir, "illegal-dir"), 0700) },
	}
	for name, check := range checks {
		t.Run("denied-"+name, func(t *testing.T) {
			if err := check(); err == nil {
				t.Fatal("read-only mutation succeeded")
			}
		})
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, files["Release [123]/space name.mkv"]) {
		t.Fatal("source file changed after denied mutations")
	}
}

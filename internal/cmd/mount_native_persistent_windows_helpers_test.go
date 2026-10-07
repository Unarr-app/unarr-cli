//go:build windows

package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/winproc"
)

type nativeWindowsPersistentFixture struct {
	f      *nativeMountFixture
	letter string
	cli    func(...string)
}

func (f *nativeWindowsPersistentFixture) filePath() string {
	return filepath.Join(f.letter+`\`, "debrid", "torbox", "Release [123]", "space name.mkv")
}

func (f *nativeWindowsPersistentFixture) read(t *testing.T) {
	t.Helper()
	r, err := os.Open(f.filePath())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := nativeReadSequence(r, f.f.files["Release [123]/space name.mkv"], 0); err != nil {
		t.Fatal(err)
	}
}

type nativeWindowsProcessIdentity struct {
	PID        int    `json:"pid"`
	ParentPID  int    `json:"parentPID"`
	Started    string `json:"started"`
	Executable string `json:"executable"`
	Name       string `json:"name"`
}

func nativeWindowsProcessSnapshot(t *testing.T) []nativeWindowsProcessIdentity {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	script := "[Console]::OutputEncoding=[Text.UTF8Encoding]::new($false); $p=@(Get-CimInstance Win32_Process | Where-Object {$_.Name -eq 'unarr.exe' -or $_.Name -eq 'rclone.exe'} | ForEach-Object {[pscustomobject]@{pid=$_.ProcessId;parentPID=$_.ParentProcessId;started=$_.CreationDate.ToUniversalTime().ToString('o');executable=$_.ExecutablePath;name=$_.Name}}); ConvertTo-Json -InputObject $p -Compress"
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", script)
	winproc.HideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("fixture process identity query: %v", err)
	}
	var result []nativeWindowsProcessIdentity
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("fixture process identity JSON: %v", err)
	}
	return result
}

func nativeWindowsOwnedPair(t *testing.T) (nativeWindowsProcessIdentity, nativeWindowsProcessIdentity) {
	t.Helper()
	processes := nativeWindowsProcessSnapshot(t)
	var daemon, child nativeWindowsProcessIdentity
	for _, p := range processes {
		if strings.EqualFold(p.Executable, os.Getenv("UNARR_NATIVE_CLI")) {
			if daemon.PID != 0 {
				t.Fatal("multiple fixture daemon identities")
			}
			daemon = p
		}
	}
	cachePath := filepath.Join(config.Dir(), "tools", filepath.Base(filepath.Dir(os.Getenv("UNARR_NATIVE_RCLONE"))), "rclone.exe")
	for _, p := range processes {
		if p.ParentPID == daemon.PID && strings.EqualFold(p.Executable, cachePath) {
			if child.PID != 0 {
				t.Fatal("multiple fixture mount identities")
			}
			child = p
		}
	}
	if daemon.PID == 0 || child.PID == 0 || daemon.Started == "" || child.Started == "" {
		t.Fatalf("verified fixture daemon/rclone pair missing: daemon=%+v child=%+v", daemon, child)
	}
	t.Logf("verified daemon PID=%d parent=%d started=%s executable=%q; rclone PID=%d parent=%d started=%s executable=%q", daemon.PID, daemon.ParentPID, daemon.Started, daemon.Executable, child.PID, child.ParentPID, child.Started, child.Executable)
	return daemon, child
}

func nativeWindowsIdentityGone(processes []nativeWindowsProcessIdentity, old nativeWindowsProcessIdentity) bool {
	for _, p := range processes {
		if p.PID == old.PID && p.Started == old.Started && strings.EqualFold(p.Executable, old.Executable) {
			return false
		}
	}
	return true
}

type nativeOldDAVPeer struct {
	conn   net.Conn
	reader *bufio.Reader
}

func nativeOpenOldDAVPeer(t *testing.T, address string) *nativeOldDAVPeer {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	request, err := http.NewRequest(http.MethodHead, "http://"+address+"/dav/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := request.Write(conn); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || response.Close {
		t.Fatalf("old DAV peer must be unauthorized but kept alive: status=%d close=%t", response.StatusCode, response.Close)
	}
	_ = conn.SetDeadline(time.Time{})
	return &nativeOldDAVPeer{conn: conn, reader: reader}
}

func (p *nativeOldDAVPeer) assertClosed(t *testing.T) {
	t.Helper()
	_ = p.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := p.reader.ReadByte()
	if err == nil {
		t.Fatal("old DAV peer remained readable")
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatal("old DAV peer survived shutdown deadline")
	}
	if !errors.Is(err, io.EOF) {
		var opErr *net.OpError
		if !errors.As(err, &opErr) {
			t.Fatalf("old DAV peer closure not established: %v", err)
		}
	}
	t.Logf("old established DAV peer closed: %v", err)
}

func nativeWindowsAssertOldPairGone(t *testing.T, daemon, child nativeWindowsProcessIdentity) {
	t.Helper()
	nativeEventually(t, 5*time.Second, "old daemon and owned mount identities gone", func() bool {
		processes := nativeWindowsProcessSnapshot(t)
		return nativeWindowsIdentityGone(processes, daemon) && nativeWindowsIdentityGone(processes, child)
	})
	t.Logf("old daemon %d and rclone %d identities gone before new mounted read", daemon.PID, child.PID)
}

func nativeWindowsStartingState(t *testing.T, daemonPID int, path string) {
	t.Helper()
	state, err := os.Stat(path)
	if err != nil || !state.IsDir() {
		t.Fatalf("old mounted drive missing: %v", err)
	}
	t.Logf("old mounted drive %q verified; expected daemon PID=%d", path, daemonPID)
}

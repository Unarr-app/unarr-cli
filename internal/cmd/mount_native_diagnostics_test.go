package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/winproc"
)

func nativeRootDiagnostics(t *testing.T, directory string) {
	t.Helper()
	if os.Getenv("UNARR_NATIVE_DIAGNOSTIC") != "1" || runtime.GOOS != "windows" {
		return
	}
	for _, op := range []struct {
		name string
		call func(string) (os.FileInfo, error)
	}{{"Stat", os.Stat}, {"Lstat", os.Lstat}} {
		info, err := op.call(directory)
		if err != nil {
			t.Logf("DIAGNOSTIC root %s: %v", op.name, err)
		} else {
			t.Logf("DIAGNOSTIC root %s: mode=%s isDir=%t size=%d", op.name, info.Mode(), info.IsDir(), info.Size())
		}
	}
	entries, err := os.ReadDir(directory)
	t.Logf("DIAGNOSTIC root ReadDir: count=%d error=%v", len(entries), err)
	for _, entry := range entries {
		t.Logf("DIAGNOSTIC root child: name=%q type=%s isDir=%t", entry.Name(), entry.Type(), entry.IsDir())
	}
	quoted := "'" + strings.ReplaceAll(directory, "'", "''") + "'"
	nativeDiagnosticCommand(t, "powershell", "-NoProfile", "-Command", "Get-Item -LiteralPath "+quoted+" | Format-List FullName,Attributes,LinkType,Target; Get-ChildItem -LiteralPath "+quoted+" -Recurse -Force | Select-Object FullName,Length,Attributes | ConvertTo-Json")
	nativeDiagnosticCommand(t, "fsutil", "reparsepoint", "query", directory)
}

func nativeDiagnosticCommand(t *testing.T, name string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	winproc.HideWindow(cmd)
	out, err := cmd.CombinedOutput()
	t.Logf("DIAGNOSTIC %s: error=%v\n%s", name, err, out)
}

// Called only before uninstall in a guarded, newly created synthetic default
// config. Never inspect an original directory restored by the outer runner.
func nativePersistentFailureDiagnostics(t *testing.T, f *nativeMountFixture, directory string) {
	t.Helper()
	if !t.Failed() || os.Getenv("UNARR_NATIVE_DIAGNOSTIC") != "1" {
		return
	}
	if !nativeSyntheticConfigDiagnostic(t, f) {
		t.Log("DIAGNOSTIC refused: synthetic default-config provenance not established")
		return
	}
	t.Logf("DIAGNOSTIC UTC=%s config=%q data=%q", time.Now().UTC().Format(time.RFC3339Nano), config.FilePath(), config.DataDir())
	nativePersistentStateDiagnostic(t, directory)
	if runtime.GOOS == "windows" {
		nativeWindowsLifecycleDiagnostic(t, f, directory, "failed before rollback")
	} else {
		nativeDiagnosticCommand(t, "systemctl", "--user", "show", "unarr.service", "-p", "MainPID", "-p", "ActiveState", "-p", "SubState")
		nativeDiagnosticCommand(t, "pgrep", "-x", "unarr")
		nativeDiagnosticCommand(t, "pgrep", "-x", "rclone")
	}
	for _, name := range []string{"unarr.log", "unarr.boot.log"} {
		t.Logf("DIAGNOSTIC synthetic %s tail:\n%s", name, tailLogFile(filepath.Join(config.DataDir(), name), 40))
	}
	// Observe readiness without converting the already-failed test into PASS.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		state, err := agent.LoadState()
		if err == nil && state.Status == "running" && agent.IsProcessAlive(state.PID) {
			t.Logf("DIAGNOSTIC registered live state appeared after failure: PID=%d status=%q", state.PID, state.Status)
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	nativePersistentStateDiagnostic(t, directory)
}

func nativeSyntheticConfigDiagnostic(t *testing.T, f *nativeMountFixture) bool {
	t.Helper()
	cfg, err := config.Load(config.FilePath())
	return err == nil && cfg.Auth.APIURL == f.web.URL
}

func nativeWindowsLifecycleDiagnostic(t *testing.T, f *nativeMountFixture, directory, phase string) {
	t.Helper()
	if runtime.GOOS != "windows" || os.Getenv("UNARR_NATIVE_DIAGNOSTIC") != "1" || !nativeSyntheticConfigDiagnostic(t, f) {
		return
	}
	t.Logf("DIAGNOSTIC lifecycle phase=%q UTC=%s", phase, time.Now().UTC().Format(time.RFC3339Nano))
	nativePersistentStateDiagnostic(t, directory)
	for _, path := range []string{agent.StopIntentPath(), agent.StartRequestPath(), config.LockPath()} {
		info, err := os.Stat(path)
		if err != nil {
			t.Logf("DIAGNOSTIC marker/lock path=%q error=%v", path, err)
		} else {
			t.Logf("DIAGNOSTIC marker/lock path=%q size=%d modified=%s (existence does not establish lock ownership)", path, info.Size(), info.ModTime().UTC().Format(time.RFC3339Nano))
		}
	}
	nativeDiagnosticCommand(t, "schtasks", "/Query", "/TN", "unarr", "/FO", "LIST", "/V")
	nativeDiagnosticCommand(t, "schtasks", "/Query", "/TN", "unarr", "/XML")
	// Inspect only fixture executables and their launcher ancestors. CommandLine
	// is deliberately excluded; unrelated process arguments can contain secrets.
	quotedCLI := "'" + strings.ReplaceAll(os.Getenv("UNARR_NATIVE_CLI"), "'", "''") + "'"
	quotedTools := "'" + strings.ReplaceAll(filepath.Join(config.Dir(), "tools")+string(os.PathSeparator), "'", "''") + "'"
	script := "$all=@(Get-CimInstance Win32_Process); $owned=@($all | Where-Object {$_.ExecutablePath -eq " + quotedCLI + " -or ($_.Name -eq 'rclone.exe' -and $_.ExecutablePath -and $_.ExecutablePath.StartsWith(" + quotedTools + ",[StringComparison]::OrdinalIgnoreCase))}); $ids=@($owned | ForEach-Object {$_.ProcessId}); for($i=0;$i -lt 3;$i++){ $parents=@($all | Where-Object {$ids -contains $_.ProcessId} | ForEach-Object {$_.ParentProcessId}); $ids+=@($all | Where-Object {$parents -contains $_.ProcessId -and ($_.Name -eq 'cmd.exe' -or $_.Name -eq 'wscript.exe')} | ForEach-Object {$_.ProcessId}) }; $all | Where-Object {$ids -contains $_.ProcessId} | Select-Object ProcessId,ParentProcessId,CreationDate,Name,ExecutablePath | ConvertTo-Json"
	nativeDiagnosticCommand(t, "powershell", "-NoProfile", "-Command", script)
}

func nativePersistentStateDiagnostic(t *testing.T, directory string) {
	t.Helper()
	state, err := agent.LoadState()
	if state == nil {
		t.Logf("DIAGNOSTIC state path=%q error=%v", agent.StateFilePath(), err)
	} else {
		t.Logf("DIAGNOSTIC state path=%q PID=%d status=%q alive=%t lastAlive=%s error=%v", agent.StateFilePath(), state.PID, state.Status, agent.IsProcessAlive(state.PID), state.LastAlive.UTC().Format(time.RFC3339Nano), err)
	}
	info, statErr := os.Stat(directory)
	mode := "unavailable"
	if info != nil {
		mode = fmt.Sprint(info.Mode())
	}
	t.Logf("DIAGNOSTIC destination=%q mode=%s error=%v", directory, mode, statErr)
}

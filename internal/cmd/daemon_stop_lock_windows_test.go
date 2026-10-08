//go:build windows

package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/gofrs/flock"
	"golang.org/x/sys/windows"
)

func TestDaemonStopLockWindowsUninstallInvalidState(t *testing.T) {
	for _, body := range []string{"malformed", "directory", "null", `{"pid":0}`, `{"pid":-1}`, `{"pid":4294967296}`} {
		t.Run(body, func(t *testing.T) {
			commands := isolateWindowsUninstallCommands(t)
			statePath := agent.StateFilePath()
			if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
				t.Fatal(err)
			}
			if body == "directory" {
				if err := os.Mkdir(statePath, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(statePath, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			command := newDaemonUninstallCmdReal()
			command.SetArgs([]string{})
			err := command.Execute()
			if err == nil || !strings.Contains(err.Error(), "cleanup incomplete") || !agent.StopIntentExists() {
				t.Fatal("unreliable state acknowledged uninstall or lost stop intent", err)
			}
			if _, err := os.Stat(filepath.Dir(config.LockPath())); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unreliable state created a replacement lock namespace", err)
			}
			if receipts, err := os.ReadFile(commands); err != nil || string(receipts) != "schtasks /end /tn unarr\n" {
				t.Fatal("unreliable state reached task/firewall removal", err, string(receipts))
			}
			if body == "directory" {
				if info, err := os.Stat(statePath); err != nil || !info.IsDir() {
					t.Fatal("state inspection error was hidden or its directory removed", err)
				}
			} else if contents, err := os.ReadFile(statePath); err != nil || string(contents) != body {
				t.Fatal("refusal changed the original state", err, string(contents))
			}
		})
	}
}

func TestDaemonStopLockWindowsUninstallExistingLock(t *testing.T) {
	isolatedMountConfig(t)
	t.Setenv("APPDATA", t.TempDir())
	t.Setenv("LOCALAPPDATA", t.TempDir())
	name := config.LockPath()
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	owner := flock.New(name)
	t.Cleanup(func() { _ = owner.Close() })
	if err := owner.Lock(); err != nil {
		t.Fatal(err)
	}
	identity := windowsLockFileIdentity(t, name)
	// Even malformed state does not alter the existing namespace path: the
	// already-held real lock, rather than state, must gate acknowledgement.
	if err := os.MkdirAll(filepath.Dir(agent.StateFilePath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agent.StateFilePath(), []byte("malformed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareMissingWindowsUninstallLockParent(); err != nil {
		t.Fatal("existing namespace preparation changed behavior", err)
	}
	requireWindowsLockIdentity(t, name, identity)
	requireWindowsLockContenderBusy(t, name)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := withStoppedDaemon(ctx, name, func(context.Context) error { called = true; return nil })
	if err == nil || called {
		t.Fatal("held existing lock did not prevent cleanup", err, called)
	}
	requireWindowsLockIdentity(t, name, identity)
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := withStoppedDaemon(context.Background(), name, func(context.Context) error {
		requireWindowsLockIdentity(t, name, identity)
		requireWindowsLockContenderBusy(t, name)
		called = true
		return nil
	}); err != nil || !called {
		t.Fatal("released existing lock did not acknowledge cleanup", err, called)
	}
	requireWindowsLockIdentity(t, name, identity)
}

func TestDaemonStopLockWindowsUninstallInvalidNamespace(t *testing.T) {
	isolatedMountConfig(t)
	t.Setenv("LOCALAPPDATA", t.TempDir())
	root := t.TempDir()
	recordWindowsLockScope(t, root)
	blocked := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blocked, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APPDATA", filepath.Join(blocked, "absent-roaming"))
	if err := prepareMissingWindowsUninstallLockParent(); err == nil {
		t.Fatal("regular-file ancestor authorized creating a lock parent")
	}
	if contents, err := os.ReadFile(blocked); err != nil || string(contents) != "preserve" {
		t.Fatal("namespace refusal changed the blocking file", err)
	}
	// No network I/O is performed: UNC syntax is rejected before attributes.
	if err := checkWindowsUninstallLockNamespace(`\\unarr-invalid-fixture\share\absent`); err == nil {
		t.Fatal("UNC namespace authorized missing-parent creation")
	}
}

// This diagnostic observes namespace movement; PASS means the probe and its
// private rollback completed, not that every attempted movement was denied.
func TestDaemonStopLockWindowsNamespace(t *testing.T) {
	for _, operation := range []string{"unlink", "parent-rename", "ancestor-rename"} {
		t.Run(operation, func(t *testing.T) {
			root := t.TempDir()
			recordWindowsLockScope(t, root)
			ancestor := filepath.Join(root, "ancestor")
			dir := filepath.Join(ancestor, "config")
			name := filepath.Join(dir, "unarr.lock")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			owner := flock.New(name)
			// Register handle release and private path restoration before any
			// mutation. Contenders are bounded and joined synchronously.
			var original, moved string
			movePending := false
			t.Cleanup(func() {
				if err := owner.Close(); err != nil {
					t.Error("release private owner", err)
				}
				if movePending {
					if err := os.Rename(moved, original); err != nil {
						t.Error("restore private namespace after releasing owner", err)
					}
				}
			})
			if err := owner.Lock(); err != nil {
				t.Fatal(err)
			}
			identity := windowsLockFileIdentity(t, name)
			t.Logf("held_file_identity=%v", identity)
			requireWindowsLockContenderBusy(t, name)
			if operation == "unlink" {
				err := os.Remove(name)
				t.Logf("held_unlink_error=%v namespace_counterexample=%v", err, err == nil)
				if err == nil {
					if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("successful unlink did not remove the private path", err)
					}
					// An unlinked inode cannot be restored through its filename.
					// Never manufacture a replacement as an acknowledgement.
					return
				}
				requireWindowsLockIdentity(t, name, identity)
				requireWindowsLockContenderBusy(t, name)
			} else {
				original, moved = dir, filepath.Join(ancestor, "moved-config")
				movedLock := filepath.Join(moved, "unarr.lock")
				if operation == "ancestor-rename" {
					original, moved = ancestor, filepath.Join(root, "moved-ancestor")
					movedLock = filepath.Join(moved, "config", "unarr.lock")
				}
				err := os.Rename(original, moved)
				movePending = err == nil
				t.Logf("held_%s_error=%v namespace_counterexample=%v", operation, err, movePending)
				if movePending {
					if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("successful movement did not remove the original path", err)
					}
					requireWindowsLockIdentity(t, movedLock, identity)
					t.Logf("original_namespace_contender=%s", windowsLockContender(t, name))
					requireWindowsLockContenderBusy(t, movedLock)
					if err := os.Rename(moved, original); err != nil {
						t.Fatal("restore private namespace while owner remains held", err)
					}
					movePending = false
				}
				requireWindowsLockIdentity(t, name, identity)
				requireWindowsLockContenderBusy(t, name)
			}
			if err := owner.Close(); err != nil {
				t.Fatal("release private owner before final deletion", err)
			}
			if result := windowsLockContender(t, name); result != "acquired" {
				t.Fatal("released private lock was not available", result)
			}
			if err := os.Remove(name); err != nil {
				t.Fatal("delete private lock after release and contender join", err)
			}
			t.Log("released_lock_deleted=true private_namespace_restored=true")
		})
	}
}

func TestDaemonStopLockWindowsNamespaceContender(t *testing.T) {
	name := os.Getenv("UNARR_TEST_WINDOWS_LOCK_CONTENDER")
	if name == "" {
		t.Skip("private bounded child process only")
	}
	lock := flock.New(name)
	locked, err := lock.TryLock()
	if err != nil {
		fmt.Printf("LOCK_RESULT=error:%v\n", err)
	} else if locked {
		fmt.Println("LOCK_RESULT=acquired")
	} else {
		fmt.Println("LOCK_RESULT=busy")
	}
	if err := lock.Close(); err != nil {
		t.Fatal("close private contender", err)
	}
}

func windowsLockContender(t *testing.T, name string) string {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "-test.run=^TestDaemonStopLockWindowsNamespaceContender$", "-test.v", "-test.timeout=8s")
	command.Env = append(os.Environ(), "UNARR_TEST_WINDOWS_LOCK_CONTENDER="+name)
	// CommandContext terminates only this exact private child on its bound;
	// CombinedOutput waits for it, so no contender survives a path mutation.
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatal("bounded private contender failed", err, string(out))
	}
	var results []string
	for _, line := range strings.Split(string(out), "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "LOCK_RESULT="); ok {
			results = append(results, value)
		}
	}
	if len(results) != 1 {
		t.Fatal("ambiguous private contender result", string(out))
	}
	return results[0]
}

func requireWindowsLockContenderBusy(t *testing.T, name string) {
	t.Helper()
	if result := windowsLockContender(t, name); result != "busy" {
		t.Fatal("separate contender did not observe the held private lock", result)
	}
}

func windowsLockFileIdentity(t *testing.T, name string) [3]uint32 {
	t.Helper()
	f, err := os.Open(name)
	if err != nil {
		t.Fatal("inspect private file identity", err)
	}
	var info windows.ByHandleFileInformation
	err = windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info)
	if err := errors.Join(err, f.Close()); err != nil {
		t.Fatal("inspect/close private identity handle", err)
	}
	// This observer handle is closed before mutation: only the flock owner's
	// sharing flags can determine whether unlink/movement is possible.
	return [3]uint32{info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow}
}

func requireWindowsLockIdentity(t *testing.T, name string, want [3]uint32) {
	t.Helper()
	if got := windowsLockFileIdentity(t, name); got != want {
		t.Fatalf("private movement changed kernel file identity: got %v want %v", got, want)
	}
}

func recordWindowsLockScope(t *testing.T, root string) {
	t.Helper()
	ptr, err := windows.UTF16PtrFromString(root)
	if err != nil {
		t.Fatal(err)
	}
	var volume [32768]uint16
	if err := windows.GetVolumePathName(ptr, &volume[0], uint32(len(volume))); err != nil {
		t.Fatal("inspect private fixture volume", err)
	}
	driveType := windows.GetDriveType(&volume[0])
	if driveType != 3 { // DRIVE_FIXED; do not probe a network fixture.
		t.Skipf("private namespace diagnostic requires a local fixed volume, got %d", driveType)
	}
	var filesystem [256]uint16
	var componentLength, flags uint32
	if err := windows.GetVolumeInformation(&volume[0], nil, 0, nil, &componentLength, &flags, &filesystem[0], uint32(len(filesystem))); err != nil {
		t.Fatal("inspect private fixture filesystem", err)
	}
	noReparse := true
	for current := root; ; current = filepath.Dir(current) {
		ptr, err := windows.UTF16PtrFromString(current)
		if err != nil {
			t.Fatal(err)
		}
		attrs, err := windows.GetFileAttributes(ptr)
		if err != nil {
			t.Fatal("inspect fixture ancestor attributes", err)
		}
		noReparse = noReparse && attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0
		t.Logf("fixture_ancestor=%q attributes=0x%x", current, attrs)
		if filepath.Dir(current) == current {
			break
		}
	}
	t.Logf("local_fixed_volume=%q filesystem=%q no_reparse_ancestors=%v filesystem_flags=0x%x", windows.UTF16ToString(volume[:]), windows.UTF16ToString(filesystem[:]), noReparse, flags)
}

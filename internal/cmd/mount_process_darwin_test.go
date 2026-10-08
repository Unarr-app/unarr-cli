package cmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"

	"golang.org/x/sys/unix"
)

func darwinOwnedTestMount(g *darwinMountOwner) unix.Statfs_t {
	m := unix.Statfs_t{Owner: uint32(os.Getuid()), Flags: unix.MNT_RDONLY, Fsid: unix.Fsid{Val: [2]int32{123, 456}}}
	copy(m.Mntonname[:], g.directory)
	copy(m.Mntfromname[:], g.device)
	copy(m.Fstypename[:], "macfuse")
	return m
}

func TestDarwinMountOwnerGuards(t *testing.T) {
	for _, scenario := range []string{"owned", "foreign-source", "foreign-uid", "foreign-type", "writable", "replaced-fsid", "replaced-parent", "replaced-leaf", "unmount-error", "latched-error"} {
		t.Run(scenario, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "parent", "mount")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			g, err := newDarwinMountOwner(dir)
			if err != nil {
				t.Fatal(err)
			}
			mounts := []unix.Statfs_t{darwinOwnedTestMount(g)}
			g.list = func() ([]unix.Statfs_t, error) { return mounts, nil }
			calls := 0
			g.unmount = func(string) error {
				calls++
				if scenario == "unmount-error" {
					return errors.New("synthetic unmount failure")
				}
				mounts = nil
				return nil
			}
			switch scenario {
			case "foreign-source":
				mounts[0].Mntfromname[0] = 'x'
			case "foreign-uid":
				mounts[0].Owner++
			case "foreign-type":
				mounts[0].Fstypename[0] = 'x'
			case "writable":
				mounts[0].Flags = 0
			case "replaced-fsid":
				if _, err := g.ownedMount(); err != nil {
					t.Fatal(err)
				}
				mounts[0].Fsid.Val[0]++
			case "replaced-parent":
				parent := filepath.Dir(g.directory)
				if err := os.Rename(parent, parent+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(g.directory, 0700); err != nil {
					t.Fatal(err)
				}
			case "replaced-leaf":
				mounts = nil
				if err := os.Rename(g.directory, g.directory+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(g.directory, 0700); err != nil {
					t.Fatal(err)
				}
			case "latched-error":
				g.err = errors.New("ownership already lost")
			}
			err = g.detach()
			if scenario == "owned" {
				if err != nil || calls != 1 {
					t.Fatalf("owned detach: calls=%d err=%v", calls, err)
				}
				return
			}
			wantCalls := 0
			if scenario == "unmount-error" {
				wantCalls = 1
			}
			if err == nil || calls != wantCalls {
				t.Fatalf("guard failed: calls=%d want=%d err=%v", calls, wantCalls, err)
			}
		})
	}
}

func TestDarwinRcloneCancellationRejectsReplacedPath(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "parent", "mount")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	t.Setenv("MOUNT_TEST_RCLONE_CHILD", "1")
	t.Setenv("MOUNT_TEST_RCLONE_PID", pidFile)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &remoteLibrary{URL: "http://127.0.0.1:1", password: "synthetic", rclone: bin}
	done := make(chan error, 1)
	go func() { done <- runRclone(ctx, s, dir) }()
	pid := awaitMountChildPID(t, pidFile)
	pgid, err := syscall.Getpgid(pid)
	if err != nil || pgid != pid || pgid == syscall.Getpgrp() {
		t.Fatalf("mount child shares supervising process group: pid=%d pgid=%d err=%v", pid, pgid, err)
	}
	parent := filepath.Dir(dir)
	if err := os.Rename(parent, parent+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "refusing replaced mount path") {
			t.Fatalf("cancellation swallowed ownership error: %v", err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("owned child cancellation unbounded")
	}
}

func TestDarwinRcloneMountNeverStartsAfterCancelledPreparation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := exec.CommandContext(ctx, "does-not-exist", "mount", "unarr:", t.TempDir())
	finish, err := configureRcloneMount(ctx, cmd, cmd.Args[3])
	if !errors.Is(err, context.Canceled) || finish != nil || cmd.Cancel == nil || len(cmd.Args) != 4 {
		t.Fatalf("cancelled preparation changed subprocess: %v", err)
	}
}

func TestDarwinMountOwnerAppearanceReobservesBeforeRefusal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mount")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	g, err := newDarwinMountOwner(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := darwinOwnedTestMount(g)
	calls := 0
	g.list = func() ([]unix.Statfs_t, error) {
		calls++
		if calls == 1 {
			// Mimic own mount hiding the original root just after absent snapshot.
			if err := os.Rename(g.directory, g.directory+"-underlying"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(g.directory, 0700); err != nil {
				t.Fatal(err)
			}
			return nil, nil
		}
		return []unix.Statfs_t{m}, nil
	}
	got, err := g.ownedMount()
	if err != nil || got == nil || g.fsid == nil || calls < 4 {
		t.Fatalf("own mount appearance incorrectly refused: mount=%v calls=%d err=%v", got, calls, err)
	}
}

func TestDarwinUnmountTimeoutReportsOwnedHelper(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "60")
	started := time.Now()
	err := waitDarwinUnmount(cmd, 30*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "cleanup incomplete") || time.Since(started) > time.Second {
		t.Fatalf("helper timeout was hidden/unbounded: %v", err)
	}
	nativeEventually(t, time.Second, "timed out helper reaped", func() bool { return !agent.IsProcessAlive(cmd.Process.Pid) })
}

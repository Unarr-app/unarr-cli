package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/gofrs/flock"
)

// Real private processes exercise lock lifetime, the existing stop watcher,
// and joining an owned child. They do not install a service or native mount.
func init() {
	switch os.Getenv("UNARR_TEST_STOP_ROLE") {
	case "mount":
		for {
			time.Sleep(time.Hour)
		}
	case "daemon":
		if err := runStopLockFixture(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
}

func runStopLockFixture() error {
	root := os.Getenv("UNARR_TEST_STOP_ROOT")
	lock := flock.New(config.LockPath())
	if err := lock.Lock(); err != nil {
		return err
	}
	defer lock.Close()
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	child := exec.Command(binary)
	child.Env = append(os.Environ(), "UNARR_TEST_STOP_ROLE=mount")
	if err := child.Start(); err != nil {
		return err
	}
	defer func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	}()
	agent.WriteState(&agent.DaemonState{PID: os.Getpid(), Status: "starting"})
	if err := os.WriteFile(filepath.Join(root, "ready"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stopIntentPoll = 25 * time.Millisecond
	signals := make(chan os.Signal, 1)
	go watchStopIntent(ctx, signals)
	select {
	case <-signals:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := os.WriteFile(filepath.Join(root, "draining"), nil, 0o600); err != nil {
		return err
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "finish-drain")); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := child.Process.Kill(); err != nil {
		return err
	}
	_ = child.Wait()
	return os.WriteFile(filepath.Join(root, "joined"), nil, 0o600)
}

func TestDaemonStopLockJoinsOwnerBeforeCleanup(t *testing.T) {
	for _, state := range []string{"starting", "missing", "stale", "changed"} {
		t.Run(state, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("UNARR_CONFIG_DIR", root)
			t.Setenv("XDG_DATA_HOME", root)
			t.Setenv("LOCALAPPDATA", root)
			t.Setenv("UNARR_TEST_STOP_ROOT", root)
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			owner := exec.CommandContext(ctx, binary)
			owner.Env = append(os.Environ(), "UNARR_TEST_STOP_ROLE=daemon")
			if err := owner.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = owner.Process.Kill(); _ = owner.Wait() })
			childPID := awaitMountChildPID(t, filepath.Join(root, "ready"))
			childProcess, err := os.FindProcess(childPID)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = childProcess.Kill()
				_ = childProcess.Release()
			})
			switch state {
			case "missing":
				agent.RemoveState()
			case "stale":
				agent.WriteState(&agent.DaemonState{PID: os.Getpid(), Status: "running"})
			}
			agent.WriteStopIntent()
			awaitStopFixtureFile(t, ctx, filepath.Join(root, "draining"))
			if state == "changed" {
				agent.WriteState(&agent.DaemonState{PID: os.Getpid(), Status: "running"})
			}
			if !agent.IsProcessAlive(owner.Process.Pid) || !agent.IsProcessAlive(childPID) {
				t.Fatal("stop intent interrupted the owner or its child before drain completed")
			}
			done := make(chan error, 1)
			cleanupReady := make(chan struct{})
			finishCleanup := make(chan struct{}, 1)
			defer close(finishCleanup)
			go func() {
				done <- stopDaemonByLock(func(context.Context) error {
					if _, err := os.Stat(filepath.Join(root, "joined")); err != nil || agent.IsProcessAlive(childPID) {
						return fmt.Errorf("cleanup acknowledged before owned child joined: %v", err)
					}
					close(cleanupReady)
					select {
					case <-finishCleanup:
					case <-ctx.Done():
						return ctx.Err()
					}
					return assertStopLockHeld(config.LockPath())
				})
			}()
			if err := os.WriteFile(filepath.Join(root, "finish-drain"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			select {
			case <-cleanupReady:
			case err := <-done:
				t.Fatal("cleanup did not reach acknowledgement", err)
			case <-ctx.Done():
				t.Fatal("cleanup did not acknowledge", ctx.Err())
			}
			if err := owner.Wait(); err != nil {
				t.Fatal("owner did not exit cleanly", err)
			}
			// The old owner's kernel lock is now certainly gone. The stopper,
			// not just the old daemon, must block replacements during cleanup.
			if err := assertStopLockHeld(config.LockPath()); err != nil {
				t.Fatal(err)
			}
			finishCleanup <- struct{}{}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("stop acknowledgement exceeded fixture bound", ctx.Err())
			}
			if state == "stale" || state == "changed" {
				if st := agent.ReadState(); st == nil || st.PID != os.Getpid() {
					t.Fatal("cleanup removed live state belonging to another process")
				}
			}
		})
	}
}

func awaitStopFixtureFile(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	for {
		if _, err := os.Stat(name); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("fixture did not reach", filepath.Base(name), ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func assertStopLockHeld(name string) error {
	contender := flock.New(name)
	defer contender.Close()
	locked, err := contender.TryLock()
	if err != nil {
		return err
	}
	if locked {
		return errors.New("replacement acquired instance lock during cleanup")
	}
	return nil
}

func TestDaemonStopLockFailurePreservesIntentAndPreventsRestart(t *testing.T) {
	for _, mode := range []string{"timeout", "cancel", "lock-error", "cleanup-error"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("UNARR_CONFIG_DIR", root)
			t.Setenv("XDG_DATA_HOME", root)
			t.Setenv("LOCALAPPDATA", root)
			agent.WriteStopIntent()
			marker := agent.StopIntentPath()
			before, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(root, "instance.lock")
			owner := flock.New(name)
			if err := owner.Lock(); err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			budget := time.Second
			if mode == "timeout" {
				budget = 25 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()
			want := error(context.DeadlineExceeded)
			switch mode {
			case "cancel":
				cancel()
				want = context.Canceled
			case "lock-error":
				name = root // a directory cannot serve as this lock file
				want = nil
			case "cleanup-error":
				if err := owner.Close(); err != nil {
					t.Fatal(err)
				}
				want = errors.New("task removal failed")
			}
			cleanupCalls, starts := 0, 0
			err = restartAfterStop(func() error {
				return withStoppedDaemon(ctx, name, func(context.Context) error {
					cleanupCalls++
					return want
				})
			}, func() error { starts++; return nil })
			if err == nil || (want != nil && !errors.Is(err, want)) || starts != 0 {
				t.Fatalf("error=%v starts=%d; wanted stop failure without restart", err, starts)
			}
			wantCalls := 0
			if mode == "cleanup-error" {
				wantCalls = 1
			}
			if cleanupCalls != wantCalls {
				t.Fatalf("destructive cleanup calls=%d; want %d", cleanupCalls, wantCalls)
			}
			if b, err := os.ReadFile(marker); err != nil || string(b) != string(before) {
				t.Fatal("failed stop removed intent", err)
			}
		})
	}
}

func TestDaemonStopLockRepeatedCleanupAndAlreadyStoppedRestart(t *testing.T) {
	name := filepath.Join(t.TempDir(), "instance.lock")
	for i := 0; i < 3; i++ {
		if err := withStoppedDaemon(context.Background(), name, func(context.Context) error { return assertStopLockHeld(name) }); err != nil {
			t.Fatal(err)
		}
	}
	starts := 0
	err := restartAfterStop(func() error { return fmt.Errorf("already stopped: %w", agent.ErrDaemonNotRunning) }, func() error { starts++; return nil })
	if err != nil || starts != 1 {
		t.Fatalf("already stopped restart: error=%v starts=%d", err, starts)
	}
}

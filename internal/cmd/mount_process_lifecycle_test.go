package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// A test executable acts as rclone without mounting a native filesystem. Child
// arguments/environment exercise the actual CommandContext cancellation path.
func init() {
	if os.Getenv("MOUNT_TEST_RCLONE_CHILD") != "1" {
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "obscure" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		fmt.Println("obscured-synthetic")
		os.Exit(0)
	}
	_ = os.WriteFile(os.Getenv("MOUNT_TEST_RCLONE_PID"), []byte(strconv.Itoa(os.Getpid())), 0o600)
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	<-interrupt
	if os.Getenv("MOUNT_TEST_RCLONE_STUCK") == "1" {
		select {}
	}
	time.Sleep(150 * time.Millisecond)
	os.Exit(0)
}

func TestRcloneCancellationJoinsDelayedChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows cancellation uses immediate process termination")
	}
	for _, stuck := range []bool{false, true} {
		t.Run(map[bool]string{false: "delayed graceful exit", true: "kill after deadline"}[stuck], func(t *testing.T) { verifyRcloneCancellation(t, stuck) })
	}
}

func verifyRcloneCancellation(t *testing.T, stuck bool) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	t.Setenv("MOUNT_TEST_RCLONE_CHILD", "1")
	t.Setenv("MOUNT_TEST_RCLONE_PID", pidFile)
	if stuck {
		t.Setenv("MOUNT_TEST_RCLONE_STUCK", "1")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &remoteLibrary{URL: "http://127.0.0.1:1", user: "synthetic", password: "synthetic", rclone: binary}
	done := make(chan error, 1)
	go func() { done <- runRclone(ctx, s, t.TempDir()) }()
	pid := awaitMountChildPID(t, pidFile)
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("child outlived bounded mount cancellation")
	}
	if agent.IsProcessAlive(pid) {
		t.Fatal("orphan rclone process", pid)
	}
	if !stuck && time.Since(start) < 100*time.Millisecond {
		t.Fatal("shutdown did not join delayed child")
	}
}

func awaitMountChildPID(t *testing.T, file string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(file)
		if err == nil {
			pid, err := strconv.Atoi(string(data))
			if err == nil {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("fake rclone never started")
	return 0
}

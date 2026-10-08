//go:build windows

package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/gofrs/flock"
)

func TestMountNativeWindowsStartingRestart(t *testing.T) {
	gates := newNativeStartupGates()
	fixture := newNativeWindowsPersistentFixture(t, gates)
	fixture.cli("mount", fixture.letter)
	nativeEventually(t, 35*time.Second, "early mounted file", func() bool {
		_, err := os.Stat(fixture.filePath())
		return err == nil
	})
	var mirrorContext context.Context
	select {
	case mirrorContext = <-gates.mirrorEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("first-start mirror request not observed")
	}
	fixture.read(t)
	oldDaemon, oldChild := nativeWindowsOwnedPair(t)
	state, err := agent.LoadState()
	if err != nil && !errors.Is(err, agent.ErrDaemonNotRunning) {
		t.Fatalf("early state read: %v", err)
	}
	if state != nil && (state.PID != oldDaemon.PID || state.Status != "starting") {
		t.Fatalf("early trigger must be starting, never running: PID=%d status=%q", state.PID, state.Status)
	}
	if state == nil {
		t.Log("early trigger: state absent")
	} else {
		t.Logf("early trigger: PID=%d status=%q", state.PID, state.Status)
	}
	lock := flock.New(config.LockPath())
	acquired, err := lock.TryLock()
	if acquired {
		_ = lock.Unlock()
	}
	if err != nil || acquired {
		t.Fatalf("early daemon must hold actual config lock: acquired=%t error=%v", acquired, err)
	}
	t.Log("actual config lock held while mounted daemon is still starting")
	nativeWindowsStartingState(t, oldDaemon.PID, fixture.letter+`\`)
	peer := nativeOpenOldDAVPeer(t, fixture.f.cfg.Mount.Listen)
	if err := mirrorContext.Err(); err != nil {
		t.Fatalf("mirror barrier expired before restart trigger: %v", err)
	}
	gates.accessPaused.Store(true)
	restart := startNativeCLI(t, "daemon", "restart")
	nativeEventually(t, 5*time.Second, "stop intent during live mirror barrier", func() bool {
		if err := mirrorContext.Err(); err != nil {
			t.Fatalf("mirror barrier expired before stop-marker observation: %v", err)
		}
		return agent.StopIntentExists()
	})
	if err := mirrorContext.Err(); err != nil {
		t.Fatalf("retained mirror request must be live at stop intent: %v", err)
	}
	t.Log("real restart stop intent observed while retained mirror request remains live")
	gates.releaseMirror()
	select {
	case <-restart.done:
		if restart.err != nil {
			t.Fatalf("starting restart: %v %s", restart.err, restart.output.String())
		}
		t.Logf("starting real CLI restart: %s", strings.TrimSpace(restart.output.String()))
	case <-time.After(35 * time.Second):
		t.Fatal("starting restart exceeded bounded deadline")
	}
	nativeWindowsAssertOldPairGone(t, oldDaemon, oldChild)
	peer.assertClosed(t)
	if _, err := os.Stat(fixture.letter + `\`); !os.IsNotExist(err) {
		t.Fatalf("old drive must be absent before permitting new activation: %v", err)
	}
	logTail := tailLogFile(filepath.Join(config.DataDir(), "unarr.log"), 80)
	if !strings.Contains(logTail, "[agent] stop requested") {
		t.Fatalf("cooperative stop marker acknowledgement absent from synthetic log:\n%s", logTail)
	}
	t.Log("old daemon/rclone/DAV peer/drive drained before releasing new generation")
	gates.releaseAll()
	nativeEventually(t, 35*time.Second, "new registered daemon after starting restart", func() bool {
		state := agent.ReadState()
		return state != nil && state.PID != oldDaemon.PID && state.Status == "running" && agent.IsProcessAlive(state.PID)
	})
	nativeEventually(t, 35*time.Second, "new real mounted file after old resources gone", func() bool {
		_, err := os.Stat(fixture.filePath())
		return err == nil
	})
	fixture.read(t)
	t.Log("actual mounted-starting restart and new exact bytes PASS; interactive consent SKIP")
}

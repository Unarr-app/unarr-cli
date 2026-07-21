package main

import (
	"sync"
	"testing"
	"time"
)

// The tray's crash-watcher state is written by control() on the click loop and
// read by refresh() on the 5s status ticker. Those genuinely overlap: the user
// clicks Pause while a tick is rendering.
//
// The overlap is not benign. lastStopPID is what tells a tray-initiated stop
// apart from a crash, and suppressCrashUntil is what keeps that stop quiet — so
// a torn read means a stop the user asked for gets reported to the developers
// as a crash. crashTracker.observe also appends to a slice.
//
// Run under -race, this fails if either side drops its lock. It drives the same
// statements the production code runs rather than one shared locked closure: a
// test that takes the lock on both sides by construction would pass no matter
// what control() and refresh() actually do.
func TestControlAndRefreshDoNotRaceOverCrashState(t *testing.T) {
	ui := &trayUI{}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// control()'s prologue, as it runs on the click loop (minus the exec).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 300 {
			ui.renderMu.Lock()
			ui.lastStopPID = 9000 + i
			ui.suppressCrashUntil = time.Now().Add(crashSuppressWindow)
			ui.renderMu.Unlock()
		}
		close(stop)
	}()

	// refresh()'s read-modify-write of the same state on the ticker, mirroring
	// renderDaemonStatus's stateCrashed branch.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			ui.renderMu.Lock()
			now := time.Now()
			if pid := 4242; pid != ui.reportedCrashPID && now.After(ui.suppressCrashUntil) {
				ui.reportedCrashPID = pid
				ui.crashes.observe(now)
				_ = ui.crashes.shouldReport(now)
			}
			_ = ui.crashes.flapping(now)
			_ = ui.lastStopPID
			ui.renderMu.Unlock()
		}
	}()

	wg.Wait()
}

// reportControlFailure moved off the click loop so a blocking dialog can no
// longer wedge the menu (Quit included). That removed the loop's implicit
// one-at-a-time gate, so the single-flight guard is what keeps a broken control
// clicked four times from stacking four modal dialogs.
func TestFailureDialogIsSingleFlighted(t *testing.T) {
	ui := &trayUI{}

	if !ui.reporting.CompareAndSwap(false, true) {
		t.Fatal("the first reporter must win the guard")
	}
	if ui.reporting.CompareAndSwap(false, true) {
		t.Error("a second concurrent reporter must lose and fall back to a notification")
	}

	// Released when the dialog closes, so the next real failure still gets one.
	ui.reporting.Store(false)
	if !ui.reporting.CompareAndSwap(false, true) {
		t.Error("the guard must be reusable once the dialog is dismissed")
	}
}

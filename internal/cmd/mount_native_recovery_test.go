package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestMountNativeDAVRecovery(t *testing.T) {
	nativeMountOptIn(t, false)
	f := newNativeMountFixture(t)
	s, err := startRemoteLibrary(context.Background(), f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	closeLibrary := func() { once.Do(s.Close) }
	t.Cleanup(closeLibrary)
	f.waitCatalog(s)
	name := "debrid/torbox/Release [123]/renewal.mkv"
	read := func() error {
		r, err := nativeDAVRequest(context.Background(), s, "GET", name, "bytes=128000-132095")
		if err != nil {
			return err
		}
		defer r.Body.Close()
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return err
		}
		if r.StatusCode != http.StatusPartialContent || !bytes.Equal(b, f.files["Release [123]/renewal.mkv"][128000:132096]) {
			return fmt.Errorf("range response status=%d bytes=%d", r.StatusCode, len(b))
		}
		return nil
	}
	if err := read(); err != nil {
		t.Fatal(err)
	}
	old := f.resolves.Load()
	f.generation.Add(1)
	if err := read(); err != nil {
		t.Fatalf("expired signed URL renewal: %v", err)
	}
	if f.resolves.Load() <= old {
		t.Fatal("expired CDN URL was not re-resolved")
	}
	f.cdnOutage.Store(1)
	if err := read(); err == nil {
		t.Fatal("synthetic CDN outage succeeded")
	}
	f.cdnOutage.Store(0)
	nativeEventually(t, 6*time.Second, "read recovery after short CDN outage", func() bool { return read() == nil })
	f.apiOutage.Store(1)
	if err := read(); err != nil {
		t.Fatalf("valid catalog/signed URL lost during API outage: %v", err)
	}
	f.apiOutage.Store(0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		r, err := nativeDAVRequest(ctx, s, "GET", "debrid/torbox/Release [123]/blocked.mkv", "bytes=0-4095")
		if err == nil {
			_, err = io.Copy(io.Discard, r.Body)
			_ = r.Body.Close()
		}
		done <- err
	}()
	defer cancel()
	select {
	case <-f.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("blocked upstream not reached")
	}
	cancel()
	select {
	case <-f.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("DAV cancellation did not reach upstream")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Error("cancelled read succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DAV read did not terminate")
	}
	nativeEventually(t, time.Second, "upstream handles closed", func() bool { return f.active.Load() == 0 })
	closeLibrary()
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if r, err := nativeDAVRequest(ctx, s, "PROPFIND", "", ""); err == nil {
		_ = r.Body.Close()
		t.Fatal("listener survived library close")
	}
	t.Logf("synthetic ranges=%d resolutions=%d; renewal, loss/recovery and cancellation exercised", f.media.Load(), f.resolves.Load())
}

func TestMountNativeKernelCancelBlockedRead(t *testing.T) {
	nativeMountOptIn(t, true)
	f := newNativeMountFixture(t)
	m := startNativeMountedSession(t, f, nativeTestDestination(t))
	done := make(chan error, 1)
	go func() {
		r, err := os.Open(filepath.Join(m.directory, "debrid", "torbox", "Release [123]", "blocked.mkv"))
		if err == nil {
			b := make([]byte, 4096)
			_, err = r.Read(b)
			_ = r.Close()
		}
		done <- err
	}()
	select {
	case <-f.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("kernel blocked read did not reach synthetic CDN")
	}
	start := time.Now()
	m.stop(t)
	select {
	case <-f.cancelled:
	case <-time.After(time.Second):
		t.Error("kernel cleanup did not cancel upstream")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Error("blocked read succeeded after mount cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Error("kernel read survived mount cancellation")
	}
	nativeEventually(t, time.Second, "no active synthetic CDN handles", func() bool { return f.active.Load() == 0 })
	t.Logf("blocked-read cancellation and cleanup took %s", time.Since(start))
	remount := startNativeMountedSession(t, f, m.directory)
	r, err := os.Open(filepath.Join(remount.directory, "debrid", "torbox", "Release [123]", "space name.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if err := nativeReadSequence(r, f.files["Release [123]/space name.mkv"], 0); err != nil {
		t.Error(err)
	}
	_ = r.Close()
	remount.stop(t)
}

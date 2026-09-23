package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// leaseProbe gives a Task a real lease on a manager with one slot in use, so a
// test can watch yield/reclaim through FreeSlots.
func leaseProbe(t *testing.T) (*Manager, *Task) {
	t.Helper()
	pr, _ := captureReporter()
	m := NewManager(ManagerConfig{MaxConcurrent: 2, OutputDir: t.TempDir()}, pr)
	task := NewTaskFromAgent(agent.Task{ID: "stall-probe-0000-0000-000000000000", Title: "probe"})
	m.running = 1
	task.slot = &slotLease{m: m, held: true}
	return m, task
}

func TestAwaitMetadataYieldsThenReclaims(t *testing.T) {
	m, task := leaseProbe(t)
	d := &TorrentDownloader{cfg: TorrentConfig{MetadataStallAfter: 20 * time.Millisecond}}
	info := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- d.awaitMetadata(context.Background(), info, task) }()

	waitUntil(t, "slot yielded after the stall threshold", func() bool { return m.FreeSlots() == 2 })
	close(info)
	if err := <-done; err != nil {
		t.Fatalf("awaitMetadata = %v, want nil once metadata arrives", err)
	}
	if got := m.FreeSlots(); got != 1 {
		t.Errorf("FreeSlots after reclaim = %d, want 1 (counts as running again)", got)
	}
}

// A timeout after yielding hands the task to a fallback method (debrid/usenet)
// that downloads for real: it must count against the cap again.
func TestAwaitMetadataTimeoutAfterYieldReclaims(t *testing.T) {
	m, task := leaseProbe(t)
	d := &TorrentDownloader{cfg: TorrentConfig{MetadataStallAfter: 10 * time.Millisecond, MetadataTimeout: 80 * time.Millisecond}}
	if err := d.awaitMetadata(context.Background(), make(chan struct{}), task); !errors.Is(err, errMetadataTimeout) {
		t.Fatalf("err = %v, want errMetadataTimeout", err)
	}
	if got := m.FreeSlots(); got != 1 {
		t.Errorf("FreeSlots after timeout = %d, want 1 (the fallback holds the slot again)", got)
	}
}

func TestAwaitMetadataFastSwarmNeverYields(t *testing.T) {
	m, task := leaseProbe(t)
	d := &TorrentDownloader{cfg: TorrentConfig{MetadataStallAfter: time.Hour}}
	info := make(chan struct{})
	close(info)
	if err := d.awaitMetadata(context.Background(), info, task); err != nil {
		t.Fatal(err)
	}
	if got := m.FreeSlots(); got != 1 {
		t.Errorf("FreeSlots = %d, want 1 (never yielded)", got)
	}
}

func TestAwaitMetadataStallDisabled(t *testing.T) {
	m, task := leaseProbe(t)
	d := &TorrentDownloader{cfg: TorrentConfig{MetadataStallAfter: -1, MetadataTimeout: 60 * time.Millisecond}}
	err := d.awaitMetadata(context.Background(), make(chan struct{}), task)
	if !errors.Is(err, errMetadataTimeout) {
		t.Fatalf("err = %v, want errMetadataTimeout", err)
	}
	if got := m.FreeSlots(); got != 1 {
		t.Errorf("FreeSlots = %d, want 1 (stall yielding disabled)", got)
	}
}

func TestAwaitMetadataTimeoutAndCancel(t *testing.T) {
	_, task := leaseProbe(t)
	d := &TorrentDownloader{cfg: TorrentConfig{MetadataTimeout: 20 * time.Millisecond}}
	if err := d.awaitMetadata(context.Background(), make(chan struct{}), task); !errors.Is(err, errMetadataTimeout) {
		t.Errorf("timeout: err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d = &TorrentDownloader{}
	if err := d.awaitMetadata(ctx, make(chan struct{}), task); !errors.Is(err, errMetadataCancelled) {
		t.Errorf("cancel: err = %v", err)
	}
}

func TestDefaultMetadataStallAfter(t *testing.T) {
	if got := (&TorrentDownloader{}).metadataStallAfter(); got != 30*time.Minute {
		t.Errorf("default stall threshold = %s, want 30m", got)
	}
}

// stallingDownloader: listed tasks yield their slot and wait for metadata
// forever (a dead magnet); the rest behave like gatedMockDownloader.
type stallingDownloader struct {
	*gatedMockDownloader
	dead map[string]bool
}

func (s *stallingDownloader) Download(ctx context.Context, t *Task, out string, p chan<- Progress) (*Result, error) {
	if s.dead[t.ID] {
		t.YieldSlot()
		<-ctx.Done()
		return nil, errMetadataCancelled
	}
	return s.gatedMockDownloader.Download(ctx, t, out, p)
}

// The 2026-09-23 NAS shape: 5 slots, the first 4 resumed torrents are dead
// magnets. Once they stall, the healthy tasks behind them must all run.
func TestStalledTorrentsDoNotBlockTheQueue(t *testing.T) {
	dir := t.TempDir()
	pr, _ := captureReporter()
	dl := &stallingDownloader{gatedMockDownloader: newGatedDownloader(dir), dead: map[string]bool{}}
	for i := range 4 {
		dl.dead[queueTaskID(i)] = true
	}
	mgr := NewManager(ManagerConfig{MaxConcurrent: 5, OutputDir: dir}, pr, dl)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go pr.Run(ctx)

	for i := range 7 {
		mgr.Submit(ctx, agent.Task{ID: queueTaskID(i), InfoHash: fmt.Sprintf("%040d", i), Title: "T", PreferredMethod: "torrent"})
	}
	waitUntil(t, "all three healthy tasks running despite 4 dead ones", func() bool { return dl.running.Load() == 3 })
	// The last dead task may still be on its way to YieldSlot when the third
	// healthy one starts (macOS CI saw FreeSlots = 1 there), so wait for the
	// slot count to settle instead of reading it once.
	waitUntil(t, "FreeSlots = 2 (3 healthy running, 4 stalled not counted)", func() bool { return mgr.FreeSlots() == 2 })
	for range 3 {
		dl.release <- struct{}{}
	}
	cancel()
	mgr.Wait()
}

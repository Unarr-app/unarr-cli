package engine

import (
	"context"
	"fmt"
	"testing"
)

// mockDownloader implements Downloader for testing.
type mockDownloader struct {
	method    DownloadMethod
	available bool
	err       error
}

func (m *mockDownloader) Method() DownloadMethod { return m.method }
func (m *mockDownloader) Available(_ context.Context, _ *Task) (bool, error) {
	return m.available, m.err
}
func (m *mockDownloader) Download(_ context.Context, _ *Task, _ string, _ chan<- Progress) (*Result, error) {
	return &Result{Method: m.method, FileName: "test.mkv", FilePath: "/tmp/test.mkv"}, nil
}
func (m *mockDownloader) Pause(_ string) error             { return nil }
func (m *mockDownloader) Cancel(_ string) error            { return nil }
func (m *mockDownloader) Shutdown(_ context.Context) error { return nil }

func TestResolveMethodAuto(t *testing.T) {
	downloaders := map[DownloadMethod]Downloader{
		MethodTorrent: &mockDownloader{method: MethodTorrent, available: true},
		MethodDebrid:  &mockDownloader{method: MethodDebrid, available: true},
	}

	task := &Task{PreferredMethod: "auto"}
	method, err := resolveMethod(context.Background(), task, downloaders, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Torrent is first in auto order
	if method != MethodTorrent {
		t.Errorf("method = %q, want torrent (first in auto order)", method)
	}
}

func TestResolveMethodSpecific(t *testing.T) {
	downloaders := map[DownloadMethod]Downloader{
		MethodTorrent: &mockDownloader{method: MethodTorrent, available: true},
		MethodDebrid:  &mockDownloader{method: MethodDebrid, available: true},
	}

	task := &Task{PreferredMethod: "debrid"}
	method, err := resolveMethod(context.Background(), task, downloaders, nil)
	if err != nil {
		t.Fatal(err)
	}
	if method != MethodDebrid {
		t.Errorf("method = %q, want debrid", method)
	}
}

func TestResolveMethodSkipsTried(t *testing.T) {
	downloaders := map[DownloadMethod]Downloader{
		MethodTorrent: &mockDownloader{method: MethodTorrent, available: true},
		MethodDebrid:  &mockDownloader{method: MethodDebrid, available: true},
	}

	task := &Task{
		PreferredMethod: "auto",
		TriedMethods:    []DownloadMethod{MethodTorrent},
	}
	method, err := resolveMethod(context.Background(), task, downloaders, nil)
	if err != nil {
		t.Fatal(err)
	}
	if method != MethodDebrid {
		t.Errorf("method = %q, want debrid (torrent already tried)", method)
	}
}

func TestResolveMethodNoneAvailable(t *testing.T) {
	downloaders := map[DownloadMethod]Downloader{
		MethodTorrent: &mockDownloader{method: MethodTorrent, available: false},
	}

	task := &Task{PreferredMethod: "auto"}
	_, err := resolveMethod(context.Background(), task, downloaders, nil)
	if err == nil {
		t.Error("expected error when no method available")
	}
}

func TestResolveMethodAvailabilityError(t *testing.T) {
	downloaders := map[DownloadMethod]Downloader{
		MethodTorrent: &mockDownloader{method: MethodTorrent, available: false, err: fmt.Errorf("network error")},
		MethodDebrid:  &mockDownloader{method: MethodDebrid, available: true},
	}

	task := &Task{ID: "test-resolve-err", PreferredMethod: "auto"}
	method, err := resolveMethod(context.Background(), task, downloaders, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Should fallback to debrid when torrent has error
	if method != MethodDebrid {
		t.Errorf("method = %q, want debrid (torrent errored)", method)
	}
}

func TestTryFallbackAutoMode(t *testing.T) {
	downloaders := map[DownloadMethod]Downloader{
		MethodTorrent: &mockDownloader{method: MethodTorrent, available: true},
		MethodDebrid:  &mockDownloader{method: MethodDebrid, available: true},
	}

	task := &Task{
		PreferredMethod: "auto",
		ResolvedMethod:  MethodTorrent,
	}

	if !tryFallback(task, downloaders, nil) {
		t.Error("should have fallback available")
	}
	if len(task.TriedMethods) != 1 || task.TriedMethods[0] != MethodTorrent {
		t.Error("torrent should be in tried methods")
	}
}

func TestTryFallbackSpecificMode(t *testing.T) {
	downloaders := map[DownloadMethod]Downloader{
		MethodTorrent: &mockDownloader{method: MethodTorrent, available: true},
		MethodDebrid:  &mockDownloader{method: MethodDebrid, available: true},
	}

	task := &Task{
		PreferredMethod: "torrent",
		ResolvedMethod:  MethodTorrent,
	}

	if tryFallback(task, downloaders, nil) {
		t.Error("should not fallback in specific mode")
	}
}

// ── config.toml preferred_methods gating ──────────────────────────────────

// Config list wins over the per-task preference: a "debrid only" agent picks
// debrid even when the task says auto AND torrent is available (the old bug:
// torrent-first auto stole every download from debrid-only users).
func TestResolveMethodConfigListWins(t *testing.T) {
	downloaders := map[DownloadMethod]Downloader{
		MethodTorrent: &mockDownloader{method: MethodTorrent, available: true},
		MethodDebrid:  &mockDownloader{method: MethodDebrid, available: true},
	}
	task := &Task{PreferredMethod: "auto"} // web said auto
	method, err := resolveMethod(context.Background(), task, downloaders, []string{"debrid"})
	if err != nil {
		t.Fatal(err)
	}
	if method != MethodDebrid {
		t.Errorf("method = %q, want debrid (config list debrid-only wins)", method)
	}
}

// A method not in the config list is NEVER used, even if it's the only one
// available — failing closed is the whole point of "debrid only".
func TestResolveMethodConfigListGatesOutTorrent(t *testing.T) {
	downloaders := map[DownloadMethod]Downloader{
		MethodTorrent: &mockDownloader{method: MethodTorrent, available: true},
		MethodDebrid:  &mockDownloader{method: MethodDebrid, available: false},
	}
	task := &Task{PreferredMethod: "auto"}
	_, err := resolveMethod(context.Background(), task, downloaders, []string{"debrid"})
	if err == nil {
		t.Error("expected error: torrent is available but not in the debrid-only list")
	}
}

// Ordered list honours order: debrid first, then usenet.
func TestResolveMethodConfigListOrder(t *testing.T) {
	downloaders := map[DownloadMethod]Downloader{
		MethodTorrent: &mockDownloader{method: MethodTorrent, available: true},
		MethodDebrid:  &mockDownloader{method: MethodDebrid, available: false},
		MethodUsenet:  &mockDownloader{method: MethodUsenet, available: true},
	}
	task := &Task{PreferredMethod: "auto"}
	method, err := resolveMethod(context.Background(), task, downloaders, []string{"debrid", "usenet"})
	if err != nil {
		t.Fatal(err)
	}
	if method != MethodUsenet {
		t.Errorf("method = %q, want usenet (debrid unavailable, next in list)", method)
	}
}

// A multi-method config list allows fallback within the list...
func TestTryFallbackConfigListMulti(t *testing.T) {
	downloaders := map[DownloadMethod]Downloader{
		MethodTorrent: &mockDownloader{method: MethodTorrent, available: true},
		MethodDebrid:  &mockDownloader{method: MethodDebrid, available: true},
	}
	task := &Task{PreferredMethod: "auto", ResolvedMethod: MethodDebrid}
	if !tryFallback(task, downloaders, []string{"debrid", "torrent"}) {
		t.Error("should fall back to torrent (in the list)")
	}
}

// ...but a single-method config list has no fallback (debrid-only never torrents).
func TestTryFallbackConfigListSingle(t *testing.T) {
	downloaders := map[DownloadMethod]Downloader{
		MethodTorrent: &mockDownloader{method: MethodTorrent, available: true},
		MethodDebrid:  &mockDownloader{method: MethodDebrid, available: true},
	}
	task := &Task{PreferredMethod: "auto", ResolvedMethod: MethodDebrid}
	if tryFallback(task, downloaders, []string{"debrid"}) {
		t.Error("debrid-only must not fall back to torrent")
	}
}

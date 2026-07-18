package engine

import (
	"context"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// vpnDropDownloader is a torrent downloader that immediately reports the VPN
// kill-switch tripped mid-download (tunnel died). Used to assert the manager PAUSES
// the task as resumable instead of hard-failing it.
type vpnDropDownloader struct{ method DownloadMethod }

func (m *vpnDropDownloader) Method() DownloadMethod                             { return m.method }
func (m *vpnDropDownloader) Available(_ context.Context, _ *Task) (bool, error) { return true, nil }
func (m *vpnDropDownloader) Download(_ context.Context, _ *Task, _ string, _ chan<- Progress) (*Result, error) {
	return nil, ErrVPNTunnelDown
}
func (m *vpnDropDownloader) Pause(_ string) error             { return nil }
func (m *vpnDropDownloader) Cancel(_ string) error            { return nil }
func (m *vpnDropDownloader) Shutdown(_ context.Context) error { return nil }

// A mid-download tunnel death on a torrent-only agent must PAUSE the task
// (resumable), NOT hard-fail it: the resume-store entry is KEPT so a daemon restart
// resumes from the partial, and the reported final state is the paused/cancelled
// state (never "failed"). This is the regression guard for the "reanudar" deliverable
// — the old code removed the store entry and reported "failed".
func TestManager_VPNTunnelDownPausesResumable(t *testing.T) {
	p := newFakePersister()
	reporter := NewProgressReporter(agent.NewClient("http://localhost", "test", "test"), time.Hour)
	mgr := NewManager(
		ManagerConfig{MaxConcurrent: 1, OutputDir: t.TempDir()},
		reporter,
		&vpnDropDownloader{method: MethodTorrent},
	)
	mgr.SetTaskStore(p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reporter.Run(ctx)

	task := dlTask("vpn-drop") // PreferredMethod "torrent" (torrent-only), InfoHash set
	mgr.Submit(ctx, task)
	if !p.has(task.ID) {
		t.Fatal("download not persisted on submit")
	}
	mgr.Wait()

	// KEY deliverable: the resume-store entry survives (a hard fail would remove it),
	// so a daemon restart re-submits and resumes from the kept pieces.
	if !p.has(task.ID) {
		t.Error("VPN-paused task removed from resume store — it would NOT resume; want kept")
	}

	// It must be reported to the web (drained into recentFinished) and NOT as failed.
	var found bool
	for _, s := range mgr.TaskStates() {
		if s.TaskID == task.ID {
			found = true
			if s.Status == "failed" {
				t.Errorf("VPN-paused task reported as %q — must be paused/resumable, not failed", s.Status)
			}
		}
	}
	if !found {
		t.Error("no final state recorded for the VPN-paused task — the web would never learn it stopped")
	}
}

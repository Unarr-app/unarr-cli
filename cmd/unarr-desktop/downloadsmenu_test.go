package main

import (
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/control"
)

// With five rows for an unbounded queue, an idle leftover must never push a
// live download out of the menu.
func TestSortForMenu_RunningFirst(t *testing.T) {
	in := []control.TaskInfo{
		{ID: "aaaaaaaa", State: "stopped"},
		{ID: "bbbbbbbb", State: "downloading", Running: true},
		{ID: "cccccccc", State: "paused"},
		{ID: "dddddddd", State: "verifying", Running: true},
	}
	got := sortForMenu(in)
	if !got[0].Running || !got[1].Running {
		t.Fatalf("running downloads did not sort first: %+v", got)
	}
	if len(got) != len(in) {
		t.Fatalf("sortForMenu dropped rows: %d in, %d out", len(in), len(got))
	}
}

func TestSortForMenu_EmptyAndNil(t *testing.T) {
	if got := sortForMenu(nil); len(got) != 0 {
		t.Fatalf("nil input produced %d rows", len(got))
	}
	if got := sortForMenu([]control.TaskInfo{}); len(got) != 0 {
		t.Fatalf("empty input produced %d rows", len(got))
	}
}

func TestDownloadRowTitle(t *testing.T) {
	cases := []struct {
		name string
		task control.TaskInfo
		want string
	}{
		{
			name: "running with a known size shows percent",
			task: control.TaskInfo{ID: "31ec4169-x", Title: "Big Movie", Running: true, Progress: 42, TotalBytes: 100},
			want: "42%  Big Movie",
		},
		{
			name: "running without a size falls back to the state",
			task: control.TaskInfo{ID: "31ec4169-x", Title: "Big Movie", Running: true, State: "resolving"},
			want: "resolving  Big Movie",
		},
		{
			name: "stopped rows lead with the state",
			task: control.TaskInfo{ID: "31ec4169-x", Title: "Big Movie", State: "paused"},
			want: "paused  Big Movie",
		},
		{
			name: "no title falls back to the file name",
			task: control.TaskInfo{ID: "31ec4169-x", FileName: "movie.mkv", State: "stopped"},
			want: "stopped  movie.mkv",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := downloadRowTitle(tc.task); got != tc.want {
				t.Errorf("downloadRowTitle = %q, want %q", got, tc.want)
			}
		})
	}
}

// A task with neither title nor filename must still be actionable, so the row
// falls back to the short id rather than rendering blank.
func TestDownloadRowTitle_FallsBackToShortID(t *testing.T) {
	got := downloadRowTitle(control.TaskInfo{ID: "31ec4169-1111-2222", State: "stopped"})
	if !strings.Contains(got, "31ec4169") {
		t.Fatalf("row title %q does not identify the task", got)
	}
}

func TestEllipsize(t *testing.T) {
	if got := ellipsize("short", 10); got != "short" {
		t.Errorf("ellipsize shortened a fitting string: %q", got)
	}
	got := ellipsize("abcdefghij", 5)
	if len([]rune(got)) != 5 || !strings.HasSuffix(got, "…") {
		t.Errorf("ellipsize = %q, want 5 runes ending in an ellipsis", got)
	}
	// Multi-byte input must be cut by runes, not bytes, or the label ends in a
	// broken character.
	multi := ellipsize("ñañañañañañaña", 5)
	if len([]rune(multi)) != 5 {
		t.Errorf("ellipsize cut multi-byte text to %d runes", len([]rune(multi)))
	}
}

func TestActionVerb(t *testing.T) {
	cases := map[string]string{
		control.ActionPause:  "paused",
		control.ActionResume: "resumed",
		control.ActionCancel: "stopped",
		control.ActionRetry:  "retried",
	}
	for action, want := range cases {
		if got := actionVerb(action); got != want {
			t.Errorf("actionVerb(%q) = %q, want %q", action, got, want)
		}
	}
}

func TestDownloadRowTooltip(t *testing.T) {
	// An error is the most useful thing the tooltip can carry.
	withErr := downloadRowTooltip(control.TaskInfo{ID: "aaaaaaaa", State: "failed", ErrorMessage: "disk full"})
	if withErr != "disk full" {
		t.Errorf("tooltip = %q, want the error message", withErr)
	}
	withMethod := downloadRowTooltip(control.TaskInfo{ID: "aaaaaaaa", State: "downloading", Method: "usenet"})
	if !strings.Contains(withMethod, "usenet") {
		t.Errorf("tooltip = %q, want the resolved method", withMethod)
	}
}

// fetchDownloads must degrade to "no rows" when there is no daemon, never
// panic or block the render path.
func TestFetchDownloads_NoDaemonIsEmpty(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir()) // no daemon.state.json in here
	if got := fetchDownloads(); len(got) != 0 {
		t.Fatalf("fetchDownloads with no daemon returned %d rows", len(got))
	}
}

func TestDownloadSlot_TaskIDRoundTrip(t *testing.T) {
	s := &downloadSlot{}
	if got := s.currentTaskID(); got != "" {
		t.Fatalf("a fresh slot reported task id %q", got)
	}
	s.taskID.Store("task-uuid-1")
	if got := s.currentTaskID(); got != "task-uuid-1" {
		t.Fatalf("currentTaskID = %q", got)
	}
}

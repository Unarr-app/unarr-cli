package engine

import (
	"fmt"
	"slices"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

func TestTaskStatesOmitsTasksWithoutAPIStatus(t *testing.T) {
	m := &Manager{
		active: map[string]*Task{
			"pending":   {ID: "pending", Status: StatusPending},
			"claimed":   {ID: "claimed", Status: StatusClaimed},
			"cancelled": {ID: "cancelled", Status: StatusCancelled},
			"running":   {ID: "running", Status: StatusDownloading},
			"seeding":   {ID: "seeding", Status: StatusSeeding},
		},
		recentFinished: []agent.TaskState{{TaskID: "done", Status: "completed"}},
	}

	var got []string
	for _, s := range m.TaskStates() {
		got = append(got, s.TaskID)
	}
	slices.Sort(got)

	if want := []string{"done", "running", "seeding"}; !slices.Equal(got, want) {
		t.Errorf("TaskStates = %v, want %v", got, want)
	}
	if len(m.recentFinished) != 0 {
		t.Errorf("recentFinished not drained: %v", m.recentFinished)
	}
}

// A long queue must not reach the server's limit of 50 tasks per sync.
func TestTaskStatesExcludesQueueFromSyncPayload(t *testing.T) {
	m := &Manager{active: map[string]*Task{}}
	for i := range 60 {
		id := fmt.Sprintf("queued-%d", i)
		m.active[id] = &Task{ID: id, Status: StatusPending}
	}
	if n := len(m.TaskStates()); n != 0 {
		t.Errorf("queued tasks in sync payload: %d, want 0", n)
	}
}

package cmd

import (
	"fmt"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// The incident's shape: 7 persisted downloads, 3 of them paused by the user.
// Only the 4 un-paused ones are queued, in store order, never force-started;
// the paused ones go back into the pause set untouched.
func TestResumeInterruptedKeepsPausesAndOrder(t *testing.T) {
	tasks := []agent.Task{
		{ID: "t1", Title: "La Brea", ResumePaused: true},
		{ID: "t2", Title: "La Brea"},
		{ID: "t3", Title: "La Brea", ResumePaused: true},
		{ID: "t4", Title: "La Brea", ResumePaused: true},
		{ID: "t5", Title: "Constantine", ForceStart: true},
		{ID: "t6", Title: "Constantine"},
		{ID: "t7", Title: "Constantine"},
	}
	var submitted, restored []string
	resumeInterrupted(tasks,
		func(t agent.Task) {
			if t.ForceStart {
				panic("boot resume must not force-start " + t.ID)
			}
			submitted = append(submitted, t.ID)
		},
		func(id string) { restored = append(restored, id) },
	)
	if fmt.Sprint(submitted) != "[t2 t5 t6 t7]" {
		t.Errorf("submitted = %v, want [t2 t5 t6 t7]", submitted)
	}
	if fmt.Sprint(restored) != "[t1 t3 t4]" {
		t.Errorf("restored paused = %v, want [t1 t3 t4]", restored)
	}
}

func TestResumeInterruptedNothingToDo(t *testing.T) {
	resumeInterrupted(nil,
		func(agent.Task) { t.Fatal("submit called with no tasks") },
		func(string) { t.Fatal("restorePaused called with no tasks") },
	)
}

package backup

import (
	"bytes"
	"context"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, time.UTC)
	if err != nil {
		panic(err)
	}
	return t
}

func TestDue(t *testing.T) {
	daily := config.BackupConfig{Enabled: true, Time: "03:00"}
	weekly := config.BackupConfig{Enabled: true, Frequency: "weekly", Weekday: "monday", Time: "03:00"}
	cases := []struct {
		name      string
		c         config.BackupConfig
		now, last string
		want      bool
	}{
		{"daily before slot, yesterday's done", daily, "2026-03-10 02:00", "2026-03-09 03:00", false},
		{"daily before slot, missed yesterday", daily, "2026-03-10 02:00", "2026-03-08 03:00", true},
		{"daily after slot, not yet", daily, "2026-03-10 03:01", "2026-03-09 03:00", true},
		{"daily after slot, done", daily, "2026-03-10 03:01", "2026-03-10 03:00", false},
		{"never backed up", daily, "2026-03-10 12:00", "", true},
		{"weekly mid-week done", weekly, "2026-03-12 10:00", "2026-03-09 03:00", false}, // 03-09 is a Monday
		{"weekly mid-week missed", weekly, "2026-03-12 10:00", "2026-03-02 03:00", true},
		{"weekly monday before slot", weekly, "2026-03-09 02:00", "2026-03-02 03:00", false},
		{"disabled", config.BackupConfig{Time: "03:00"}, "2026-03-10 12:00", "", false},
		{"bad time never fires", config.BackupConfig{Enabled: true, Time: "25:99"}, "2026-03-10 12:00", "", false},
	}
	for _, c := range cases {
		var last time.Time
		if c.last != "" {
			last = at(c.last)
		}
		if got := due(c.c, at(c.now), last); got != c.want {
			t.Errorf("%s: due = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSchedulerWritesAndPrunes(t *testing.T) {
	l := newLayout(t)
	dir := t.TempDir()
	now := at("2026-03-10 04:00")
	s := &Scheduler{Config: config.BackupConfig{Enabled: true, Time: "03:00", RetentionCount: 2, Dir: dir},
		Layout: l, Version: "t", Now: func() time.Time { return now }}

	// Three consecutive days → three due runs; retention keeps the newest two.
	for i := 0; i < 3; i++ {
		s.tick()
		s.tick() // second tick the same day must be a no-op
		now = now.AddDate(0, 0, 1)
	}
	es, err := List(dir)
	if err != nil || len(es) != 2 {
		t.Fatalf("entries after retention: %v %v", es, err)
	}
	if es[0].CreatedAt.Day() != 12 || es[1].CreatedAt.Day() != 11 || es[0].Kind != KindScheduled {
		t.Fatalf("kept wrong backups: %+v %+v", es[0], es[1])
	}
	// Scheduled archives never carry secrets.
	arc, err := Open(es[0].Path, "")
	if err != nil || arc.Manifest.Secrets != SecretsRedacted {
		t.Fatalf("scheduled archive: %v %+v", err, arc)
	}
}

// Automatic retention must never delete what the user made by hand — least of
// all an encrypted archive, the one copy that holds the secrets.
func TestSchedulerNeverPrunesManualBackups(t *testing.T) {
	l := newLayout(t)
	dir := t.TempDir()
	manual, err := Create(CreateOptions{Layout: l, Dir: dir, Passphrase: "pw", Now: at("2026-03-09 12:00")})
	if err != nil {
		t.Fatal(err)
	}
	now := at("2026-03-10 04:00")
	s := &Scheduler{Config: config.BackupConfig{Enabled: true, Time: "03:00", RetentionCount: 1, Dir: dir},
		Layout: l, Now: func() time.Time { return now }}
	for i := 0; i < 4; i++ {
		s.tick()
		now = now.AddDate(0, 0, 1)
	}
	es, _ := List(dir)
	kinds := map[Kind]int{}
	for _, e := range es {
		kinds[e.Kind]++
		if e.Path == manual.Path && !e.Encrypted {
			t.Fatal("manual archive lost its encryption tag")
		}
	}
	if kinds[KindManual] != 1 || kinds[KindScheduled] != 1 {
		t.Fatalf("kinds after 4 scheduled runs with retention 1: %v (manual must survive)", kinds)
	}
}

// A persistent failure is retried after the backoff, not every minute.
func TestSchedulerBacksOffAfterFailure(t *testing.T) {
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })

	empty := Layout{ConfigPath: filepath.Join(t.TempDir(), "none.toml"), DataDir: t.TempDir()} // ErrNothingToBackup
	now := at("2026-03-10 04:00")
	s := &Scheduler{Config: config.BackupConfig{Enabled: true, Dir: t.TempDir()}, Layout: empty,
		Now: func() time.Time { return now }}
	for i := 0; i < 30; i++ { // half an hour of one-minute ticks
		s.tick()
		now = now.Add(time.Minute)
	}
	if n := strings.Count(logs.String(), "scheduled backup failed"); n != 1 {
		t.Fatalf("failed attempts in 30 min = %d, want 1 (backoff)\n%s", n, logs.String())
	}
	now = now.Add(retryBackoff)
	s.tick()
	if n := strings.Count(logs.String(), "scheduled backup failed"); n != 2 {
		t.Fatalf("no retry after the backoff: %d attempts", n)
	}
}

func TestSchedulerRunStopsOnCancel(t *testing.T) {
	s := &Scheduler{Config: config.BackupConfig{Dir: filepath.Join(t.TempDir(), "b")}, Interval: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

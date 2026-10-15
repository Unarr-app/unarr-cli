package backup

import (
	"context"
	"log"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

// retryBackoff is how long the scheduler waits after a failed backup before
// trying again. Without it a persistent failure (unwritable dir, a config that
// cannot be redacted) is retried — and logged — every minute, forever.
const retryBackoff = time.Hour

// lastSlot returns the most recent scheduled instant at or before now, in
// now's location. Daily → today (or yesterday) at HH:MM; weekly → the latest
// configured weekday at HH:MM.
func lastSlot(c config.BackupConfig, now time.Time) time.Time {
	h, m, _ := c.ClockTime()
	slot := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if c.Weekly() {
		slot = slot.AddDate(0, 0, -int((now.Weekday()-c.WeekdayOrDefault()+7)%7))
	}
	if slot.After(now) {
		if c.Weekly() {
			return slot.AddDate(0, 0, -7)
		}
		return slot.AddDate(0, 0, -1)
	}
	return slot
}

// due reports whether a scheduled backup is owed: the schedule is on and no
// backup exists newer than the most recent slot. Stateless on purpose — the
// newest file in the backup dir is the state — so a daemon that was off at 03:00
// catches up at its next start instead of skipping the day.
func due(c config.BackupConfig, now, lastBackup time.Time) bool {
	if !c.Enabled {
		return false
	}
	if _, _, err := c.ClockTime(); err != nil {
		return false
	}
	return lastBackup.Before(lastSlot(c, now))
}

// Scheduler runs scheduled backups from inside the daemon. The table is read
// once: like every other setting, changing it needs a daemon restart.
type Scheduler struct {
	Config config.BackupConfig
	Layout Layout
	// Version is stamped into the manifest.
	Version string
	// Interval between due-checks (default 1 minute).
	Interval time.Duration
	Now      func() time.Time

	retryAt time.Time
}

// Run blocks until ctx is done. Failures are logged, never fatal: a backup
// problem must not take the daemon down.
func (s *Scheduler) Run(ctx context.Context) {
	if s.Interval <= 0 {
		s.Interval = time.Minute
	}
	if s.Now == nil {
		s.Now = time.Now
	}
	t := time.NewTicker(s.Interval)
	defer t.Stop()
	s.tick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tick()
		}
	}
}

func (s *Scheduler) tick() {
	now := s.Now()
	if now.Before(s.retryAt) {
		return
	}
	dir := s.Config.Directory(s.Layout.ConfigPath)
	if !due(s.Config, now, newestBackup(dir)) {
		return
	}
	res, err := create(CreateOptions{Layout: s.Layout, Version: s.Version, Dir: dir, Now: now}, KindScheduled)
	if err != nil {
		s.retryAt = now.Add(retryBackoff)
		log.Printf("[backup] scheduled backup failed (next try in %s): %v", retryBackoff, err)
		return
	}
	s.retryAt = time.Time{}
	log.Printf("[backup] wrote %s", res.Path)
	if removed, err := pruneScheduled(dir, s.Config.Retention()); err != nil {
		log.Printf("[backup] prune: %v", err)
	} else if len(removed) > 0 {
		log.Printf("[backup] pruned %d old scheduled backup(s)", len(removed))
	}
}

// newestBackup is the creation time of the newest manual or scheduled archive
// in dir (zero when there is none): either kind satisfies the schedule.
func newestBackup(dir string) time.Time {
	es, err := List(dir)
	if err != nil {
		return time.Time{}
	}
	for _, e := range es {
		if e.Kind != KindPreRestore {
			return e.CreatedAt
		}
	}
	return time.Time{}
}

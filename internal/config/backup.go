package config

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Schedule frequencies accepted by [backup] frequency.
const (
	BackupDaily  = "daily"
	BackupWeekly = "weekly"
)

// Defaults for the scheduled state backup. They live in accessors rather than
// in Default(): a zero BackupConfig is omitted by Save (omitempty), so an
// existing config.toml is not rewritten with a [backup] table nobody asked for.
const (
	defaultBackupTime      = "03:00"
	defaultBackupRetention = 3
)

// BackupConfig schedules automatic snapshots of the local state (config.toml,
// the download resume queue). Off by default. Secrets are never part of a
// scheduled backup: they would sit in cleartext on disk with nobody to type a
// passphrase, so the scheduler always runs in the redacted mode.
type BackupConfig struct {
	Enabled bool `toml:"enabled"`
	// Frequency is "daily" or "weekly" (default "daily").
	Frequency string `toml:"frequency,omitempty"`
	// Time is the local wall-clock time "HH:MM" the backup is due (default "03:00").
	Time string `toml:"time,omitempty"`
	// Weekday applies to frequency = "weekly" only (default "sunday").
	Weekday string `toml:"weekday,omitempty"`
	// RetentionCount is how many SCHEDULED backups the scheduler keeps (default
	// 3). Manual and pre-restore archives are never deleted automatically.
	RetentionCount int `toml:"retention_count,omitempty"`
	// Dir overrides where archives are stored (default <config dir>/backups).
	Dir string `toml:"dir,omitempty"`
}

// Weekly reports whether the schedule runs once a week rather than daily.
func (b BackupConfig) Weekly() bool {
	return strings.EqualFold(strings.TrimSpace(b.Frequency), BackupWeekly)
}

// TimeOrDefault returns the configured "HH:MM" or the default.
func (b BackupConfig) TimeOrDefault() string {
	if t := strings.TrimSpace(b.Time); t != "" {
		return t
	}
	return defaultBackupTime
}

// WeekdayOrDefault returns the configured weekday or the default.
func (b BackupConfig) WeekdayOrDefault() time.Weekday {
	if w, ok := parseWeekday(b.Weekday); ok {
		return w
	}
	return time.Sunday
}

// Retention returns how many scheduled backups to keep (always >= 1).
func (b BackupConfig) Retention() int {
	if b.RetentionCount > 0 {
		return b.RetentionCount
	}
	return defaultBackupRetention
}

// Directory returns where backups are written for the config file at
// configPath: the explicit dir, else DefaultBackupDir(configPath).
func (b BackupConfig) Directory(configPath string) string {
	if d := strings.TrimSpace(b.Dir); d != "" {
		return d
	}
	return DefaultBackupDir(configPath)
}

// DefaultBackupDir is "backups" beside config.toml. Derived from the config
// path actually in use (like MountConfig.NZBDirectory) so a second agent run
// with --config never shares, prunes or schedules off another agent's archives.
// It sits beside config.toml rather than in the data dir because
// `unarr clean --all` removes the data dir on Linux and Windows.
func DefaultBackupDir(configPath string) string {
	if configPath == "" {
		configPath = FilePath()
	}
	return filepath.Join(filepath.Dir(configPath), "backups")
}

// ClockTime parses Time ("HH:MM") into hour and minute.
func (b BackupConfig) ClockTime() (hour, minute int, err error) {
	return parseClock(b.TimeOrDefault())
}

// Validate returns the first problem `unarr config check` reports for this
// table, or nil. The daemon refuses to schedule an invalid table instead of
// silently guessing what "3am" or "funday" meant.
func (b BackupConfig) Validate() error {
	if iss := b.valueIssues(); len(iss) > 0 {
		return fmt.Errorf("%s", iss[0].Message)
	}
	return nil
}

func parseClock(s string) (int, int, error) {
	h, m, ok := strings.Cut(s, ":")
	hour, errH := strconv.Atoi(h)
	minute, errM := strconv.Atoi(m)
	if !ok || errH != nil || errM != nil || len(h) != 2 || len(m) != 2 ||
		hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("%q is not a time of day (use 24h \"HH:MM\", e.g. \"03:00\")", s)
	}
	return hour, minute, nil
}

func parseWeekday(s string) (time.Weekday, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	for d := time.Sunday; d <= time.Saturday; d++ {
		if strings.ToLower(d.String()) == s {
			return d, true
		}
	}
	return 0, false
}

// valueIssues reports settings the scheduler would otherwise silently ignore.
func (b BackupConfig) valueIssues() issueList {
	var l issueList
	l.oneOf("backup.frequency", b.Frequency, BackupDaily, BackupWeekly)
	if strings.TrimSpace(b.Time) != "" {
		if _, _, err := parseClock(strings.TrimSpace(b.Time)); err != nil {
			l.add("backup.time", err.Error())
		}
	}
	if strings.TrimSpace(b.Weekday) != "" {
		if _, ok := parseWeekday(b.Weekday); !ok {
			l.add("backup.weekday", fmt.Sprintf("%q is not a weekday (sunday … saturday)", b.Weekday))
		}
	}
	if b.RetentionCount < 0 {
		l.add("backup.retention_count", fmt.Sprintf("%d must be zero or positive (0 = default %d)", b.RetentionCount, defaultBackupRetention))
	}
	if b.Dir != "" && !filepath.IsAbs(b.Dir) {
		l.add("backup.dir", "must be an absolute path")
	}
	return l
}

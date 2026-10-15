package backup

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Entry is a backup file found in a directory. Metadata comes from the file
// name alone, so listing works for encrypted archives without a passphrase.
type Entry struct {
	Path      string
	Name      string
	Size      int64
	CreatedAt time.Time
	Encrypted bool
	Kind      Kind
}

// List returns the backups in dir, newest first. A missing dir is an empty list.
func List(dir string) ([]Entry, error) {
	des, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, de := range des {
		e, ok := parseEntryName(de.Name())
		if !ok || !de.Type().IsRegular() {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		e.Path, e.Size = filepath.Join(dir, de.Name()), info.Size()
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b Entry) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out, nil
}

// parseEntryName understands "<kind prefix><stamp>.zip" and ".zip.enc".
func parseEntryName(name string) (Entry, bool) {
	e := Entry{Name: name}
	rest, found := "", false
	for _, k := range allKinds {
		if r, ok := strings.CutPrefix(name, k.prefix()); ok {
			rest, e.Kind, found = r, k, true
			break
		}
	}
	if !found {
		return e, false
	}
	switch {
	case strings.HasSuffix(rest, extEnc):
		rest, e.Encrypted = strings.TrimSuffix(rest, extEnc), true
	case strings.HasSuffix(rest, extZip):
		rest = strings.TrimSuffix(rest, extZip)
	default:
		return e, false
	}
	t, err := time.Parse(stampLayout, rest)
	if err != nil {
		return e, false
	}
	e.CreatedAt = t
	return e, true
}

// Prune deletes all but the newest keep manual and scheduled backups in dir —
// the explicit `unarr backup prune`. Pre-restore snapshots have their own
// retention (PrunePreRestore).
func Prune(dir string, keep int) ([]string, error) {
	return prune(dir, keep, func(e Entry) bool { return e.Kind != KindPreRestore }, nil)
}

// pruneScheduled is the scheduler's retention: it only ever deletes archives
// the scheduler wrote, so an encrypted manual backup — the one copy that holds
// the secrets — cannot age out behind the user's back.
func pruneScheduled(dir string, keep int) ([]string, error) {
	return prune(dir, keep, func(e Entry) bool { return e.Kind == KindScheduled }, nil)
}

// Pre-restore retention. A snapshot is the undo button for a restore AND a
// cleartext copy of this machine's secrets, so it must neither go early nor
// pile up forever: the newest PreRestoreKeep always stay, and an older one only
// goes once it is PreRestoreMaxAge old.
const (
	PreRestoreKeep   = 3
	PreRestoreMaxAge = 30 * 24 * time.Hour
)

// PrunePreRestore applies the pre-restore retention to the snapshots in dir as
// of now. spare is never deleted: it is the snapshot a restore has just taken.
func PrunePreRestore(dir string, now time.Time, spare string) ([]string, error) {
	return prune(dir, PreRestoreKeep,
		func(e Entry) bool { return e.Kind == KindPreRestore },
		func(e Entry) bool { return e.Path == spare || now.Sub(e.CreatedAt) < PreRestoreMaxAge })
}

// prune deletes the eligible archives in dir past the newest keep, except those
// holdBack (nil = none) reports.
func prune(dir string, keep int, eligible, holdBack func(Entry) bool) ([]string, error) {
	if keep < 1 {
		keep = 1 // never delete the last copy
	}
	entries, err := List(dir)
	if err != nil {
		return nil, err
	}
	var removed []string
	n := 0
	for _, e := range entries {
		if !eligible(e) {
			continue
		}
		if n++; n <= keep || (holdBack != nil && holdBack(e)) {
			continue
		}
		if err := os.Remove(e.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return removed, err
		}
		removed = append(removed, e.Path)
	}
	return removed, nil
}

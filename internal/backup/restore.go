package backup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Item actions in a restore plan.
const (
	ActionCreate    = "create"
	ActionOverwrite = "overwrite"
	ActionUnchanged = "unchanged"
)

// PlanItem is one file a restore would touch.
type PlanItem struct {
	Name, Dest, Action string
	Size               int64
	data               []byte
	prev               []byte // content on disk when planned, for rollback
	existed            bool   // false: Dest was absent, so a rollback removes it
}

// Plan is what a restore does (or did).
type Plan struct {
	Manifest Manifest
	Items    []PlanItem
	Warnings []string
	// PreRestore is the snapshot taken before overwriting (Apply only; empty
	// when there was no previous state to save).
	PreRestore string
	Applied    bool
	layout     Layout
}

// Changes reports whether applying the plan would write anything.
func (p *Plan) Changes() bool {
	for _, it := range p.Items {
		if it.Action != ActionUnchanged {
			return true
		}
	}
	return false
}

// Plan computes what restoring the archive onto l would change. It writes
// nothing. A caller about to Apply must plan while already holding the daemon's
// instance lock, so the state it compares against cannot change underneath.
func (a *Archive) Plan(l Layout) (Plan, error) {
	p := Plan{Manifest: a.Manifest, layout: l}
	if a.Manifest.OS != runtime.GOOS {
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"archive was made on %s, this is %s: check paths such as downloads.dir after restoring", a.Manifest.OS, runtime.GOOS))
	}
	for _, e := range a.Manifest.Files {
		it, warn, err := a.planItem(l, e.Name)
		if err != nil {
			return p, err
		}
		p.Items = append(p.Items, it)
		p.Warnings = append(p.Warnings, warn...)
	}
	if a.Manifest.Secrets == SecretsRedacted {
		creds := "the credentials already on this machine are kept"
		if !p.overwrites(nameConfig) {
			creds = "there is no config here to keep them from, so run `unarr login` after restoring"
		}
		p.Warnings = append(p.Warnings, "archive has no secrets: "+creds+
			"; debrid downloads in the queue have no link of their own and need a fresh one from the server")
	}
	return p, nil
}

// overwrites reports whether the plan replaces an existing file named name.
func (p *Plan) overwrites(name string) bool {
	for _, it := range p.Items {
		if it.Name == name {
			return it.existed
		}
	}
	return false
}

func (a *Archive) planItem(l Layout, name string) (PlanItem, []string, error) {
	dest, ok := l.dest(name)
	if !ok {
		return PlanItem{}, nil, fmt.Errorf("%w: %q is not a file unarr restores", ErrUnsafePath, name)
	}
	// Write THROUGH a symlink (a dotfiles-managed config.toml), not over it:
	// renaming onto the link would silently replace it with a regular file.
	if real, err := filepath.EvalSymlinks(dest); err == nil {
		dest = real
	}
	it := PlanItem{Name: name, Dest: dest, Action: ActionCreate, data: a.files[name]}
	cur, err := os.ReadFile(dest)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return it, nil, fmt.Errorf("read current %s: %w", dest, err)
	default:
		it.Action, it.prev, it.existed = ActionOverwrite, cur, true
	}
	var warn []string
	if it.existed && name == nameConfig && a.Manifest.Secrets == SecretsRedacted {
		it.data, warn = mergeRedactedConfig(it.data, cur)
	}
	if it.existed && sumHex(it.prev) == sumHex(it.data) {
		it.Action = ActionUnchanged
	}
	it.Size = int64(len(it.data))
	return it, warn, nil
}

// ApplyOptions configures Plan.Apply.
type ApplyOptions struct {
	// SnapshotDir receives the pre-restore snapshot of the current state. It
	// holds secrets in the clear, so it must be local (beside config.toml).
	SnapshotDir string
	Version     string
	Now         time.Time
}

// Apply executes the plan: snapshot the current state, then replace each file
// atomically. A failed snapshot changes nothing; a failed write puts back the
// files already replaced, so a restore lands whole or not at all. The caller
// holds the daemon's instance lock for the whole Plan+Apply.
func (p *Plan) Apply(o ApplyOptions) error {
	if !p.Changes() {
		return nil
	}
	snap, err := create(CreateOptions{Layout: p.layout, Version: o.Version, Dir: o.SnapshotDir, Now: o.Now}, KindPreRestore)
	if err != nil && !errors.Is(err, ErrNothingToBackup) {
		return fmt.Errorf("pre-restore snapshot (nothing was changed): %w", err)
	}
	p.PreRestore = snap.Path
	var written []PlanItem
	for _, it := range p.Items {
		if it.Action == ActionUnchanged {
			continue
		}
		if err := writeFileAtomic(it.Dest, it.data); err != nil {
			return p.rollback(written, fmt.Errorf("write %s: %w", it.Dest, err))
		}
		written = append(written, it)
	}
	p.Applied = true
	return nil
}

// rollback restores the files already written, newest first, and reports cause
// together with whatever could not be put back.
func (p *Plan) rollback(written []PlanItem, cause error) error {
	errs := []error{cause}
	for i := len(written) - 1; i >= 0; i-- {
		it := written[i]
		var err error
		if it.existed {
			err = writeFileAtomic(it.Dest, it.prev)
		} else {
			err = os.Remove(it.Dest)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("roll back %s: %w", it.Dest, err))
		}
	}
	if len(errs) == 1 {
		return fmt.Errorf("restore failed, previous files put back: %w", cause)
	}
	if p.PreRestore != "" {
		errs = append(errs, fmt.Errorf("the previous state is saved in %s", p.PreRestore))
	}
	return fmt.Errorf("restore failed and could not be fully rolled back: %w", errors.Join(errs...))
}

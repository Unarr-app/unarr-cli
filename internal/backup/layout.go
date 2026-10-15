package backup

import (
	"os"
	"path/filepath"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/fsx"
)

// Archive member names. They double as the restore allowlist.
const (
	nameConfig = "config.toml"
	nameTasks  = "active-tasks.json"
)

// Layout says where the state lives. Injected so tests run in a temp dir and
// `--config` pointing at a non-default config.toml is honoured.
type Layout struct {
	ConfigPath string // config.toml
	DataDir    string // holds active-tasks.json
}

// source is one allowlisted file: its archive name, where it lives, how to
// strip secrets from it, and how to tell that unarr can load it.
type source struct {
	name   string
	path   string
	redact func([]byte) ([]byte, error)
	check  func([]byte) error
}

func (l Layout) sources() []source {
	return []source{
		{nameConfig, l.ConfigPath, redactConfig, checkConfig},
		{nameTasks, filepath.Join(l.DataDir, nameTasks), redactTasks, checkTasks},
	}
}

// source maps an archive member to its allowlisted file; ok is false for any
// name not on the allowlist, which is what makes path traversal impossible.
func (l Layout) source(name string) (source, bool) {
	for _, s := range l.sources() {
		if s.name == name {
			return s, true
		}
	}
	return source{}, false
}

// Bounds for the rename retry: a Windows antivirus or indexer briefly holding a
// file we just wrote must not fail a restore halfway through.
const (
	renameWindow = 2 * time.Second
	renameStep   = 50 * time.Millisecond
)

// isTransientBlock is the retry criterion; a var so tests can stand in for
// Windows, the only platform where it ever reports true.
var isTransientBlock = fsx.IsTransientRenameBlock

// retryTransient is the read side of writeFileAtomic's rename retry: it calls
// read until it succeeds, fails for any other reason, or window runs out,
// pausing step between attempts, and returns the last error. On Windows an
// open of active-tasks.json that lands while the daemon replaces it by rename
// fails with a sharing violation that clears within milliseconds.
func retryTransient(read func() ([]byte, error), window, step time.Duration) ([]byte, error) {
	deadline := time.Now().Add(window)
	for {
		data, err := read()
		if err == nil || !isTransientBlock(err) || !time.Now().Before(deadline) {
			return data, err
		}
		time.Sleep(step)
	}
}

// writeFileAtomic writes via a temp file in the destination directory then
// renames it over the target, so a crash never leaves a half-written file.
// os.CreateTemp creates the file 0600, so a copy of config.toml (or of its
// secrets) is never readable by other users, even for an instant.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(err error) error {
		_ = tmp.Close() // already closed on the rename path; the error is moot
		_ = os.Remove(name)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	if err := fsx.RenameWithRetry(name, path, renameWindow, renameStep); err != nil {
		return fail(err)
	}
	return nil
}

package backup

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	extZip      = ".zip"
	extEnc      = ".zip.enc"
	stampLayout = "20060102T150405Z"
)

// Kind says who made an archive, which decides who may delete it: automatic
// pruning only ever removes what the scheduler itself wrote.
type Kind int

const (
	// KindManual is `unarr backup create`; only `unarr backup prune` deletes it.
	KindManual Kind = iota
	// KindScheduled is the daemon's [backup] schedule, which prunes its own.
	KindScheduled
	// KindPreRestore is the undo button for a restore. Only its own retention
	// (PrunePreRestore, after a restore that succeeded) deletes it.
	KindPreRestore
)

var allKinds = []Kind{KindManual, KindScheduled, KindPreRestore}

// prefix is the file-name prefix that records the kind on disk, so List needs
// no index file and works on encrypted archives without a passphrase.
func (k Kind) prefix() string {
	switch k {
	case KindScheduled:
		return "unarr-scheduled-"
	case KindPreRestore:
		return "unarr-prerestore-"
	default:
		return "unarr-backup-"
	}
}

// CreateOptions configures Create.
type CreateOptions struct {
	Layout  Layout
	Version string
	// Out is a file path, or an existing/trailing-separator directory in which
	// a timestamped name is generated. Empty → Dir.
	Out string
	Dir string
	// Passphrase, when set, encrypts the archive and keeps secrets in it.
	// Empty → secrets are redacted and the archive is a plain zip.
	Passphrase string
	Now        time.Time
}

// Result describes a created archive.
type Result struct {
	Path     string
	Manifest Manifest
}

// Create writes a manual backup archive atomically (tmp+rename, 0600). It can
// only produce an encrypted archive or a secret-free one.
func Create(o CreateOptions) (Result, error) {
	return create(o, KindManual)
}

func create(o CreateOptions, k Kind) (Result, error) {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	o.Now = o.Now.UTC()
	secrets := SecretsRedacted
	// The pre-restore snapshot keeps secrets in the clear: it is a copy of files
	// already on this disk, and it is written beside them, never to [backup] dir.
	if o.Passphrase != "" || k == KindPreRestore {
		secrets = SecretsIncluded
	}

	// A pre-restore snapshot is not checked: it is the undo for a restore, and
	// refusing a config that does not parse would block restoring a good backup
	// over a broken config — the very case restore is for.
	files, err := collect(o.Layout, secrets == SecretsRedacted, k != KindPreRestore)
	if err != nil {
		return Result{}, err
	}
	if len(files) == 0 {
		return Result{}, ErrNothingToBackup
	}
	m := Manifest{
		Schema: SchemaVersion, UnarrVersion: o.Version, OS: runtime.GOOS, Arch: runtime.GOARCH,
		CreatedAt: o.Now, Secrets: secrets,
	}
	for _, f := range files {
		m.Files = append(m.Files, FileEntry{Name: f.name, Size: int64(len(f.data)), SHA256: sumHex(f.data)})
	}
	blob, err := pack(m, files, o.Passphrase)
	if err != nil {
		return Result{}, err
	}
	path := outputPath(o, k)
	if err := writeFileAtomic(path, blob); err != nil {
		return Result{}, fmt.Errorf("write backup: %w", err)
	}
	return Result{Path: path, Manifest: m}, nil
}

type memFile struct {
	name string
	data []byte
}

// collect reads the allowlisted files that exist, checking that restore would
// accept them and redacting if asked. A read that Windows blocks for a moment —
// the daemon replacing the queue file by rename — is retried within the same
// bounds as writeFileAtomic's rename.
func collect(l Layout, redact, check bool) ([]memFile, error) {
	var out []memFile
	for _, s := range l.sources() {
		data, err := retryTransient(func() ([]byte, error) { return readSource(s) }, renameWindow, renameStep)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err == nil {
			data, err = s.prepare(data, redact, check)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, memFile{s.name, data})
	}
	return out, nil
}

// prepare turns a file read from disk into its archive member. The check comes
// first, so a config that does not parse is reported as such, not as one whose
// secrets cannot be stripped; what redaction then writes parses by construction.
func (s source) prepare(data []byte, redact, check bool) ([]byte, error) {
	if check {
		if err := s.check(data); err != nil {
			return nil, err
		}
	}
	if !redact {
		return data, nil
	}
	out, err := s.redact(data)
	if err != nil {
		return nil, fmt.Errorf("redact %s: %w", s.name, err)
	}
	return out, nil
}

// openFile is os.Open; a var so tests can fail an open the way Windows does.
var openFile = os.Open

// readSource reads one allowlisted file, refusing one larger than a backup can
// hold — its own cap, or the shared bound — before reading it into memory.
func readSource(s source) ([]byte, error) {
	f, err := openFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.path, err)
	}
	defer f.Close()
	budget := newContentBudget() // what the file may take in an otherwise empty backup
	room := budget.room(s.name)
	if st, err := f.Stat(); err == nil && st.Size() > room {
		return nil, budget.tooLarge(s.name, st.Size())
	}
	data, over, err := readLimited(f, room)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.path, err)
	}
	if over {
		return nil, budget.tooLarge(s.name, -1)
	}
	return data, nil
}

// pack builds the archive in memory — encryption seals the whole zip in one
// go — and refuses, before anything is written, one that Open would refuse.
func pack(m Manifest, files []memFile, passphrase string) ([]byte, error) {
	mj, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	members := append([]memFile{{ManifestName, mj}}, files...)
	budget := newContentBudget()
	for _, f := range members {
		if err := budget.spend(f.name, int64(len(f.data))); err != nil {
			return nil, err
		}
	}
	blob, err := buildZip(members, m.CreatedAt)
	if err != nil {
		return nil, err
	}
	if passphrase != "" {
		if blob, err = encrypt(blob, passphrase); err != nil {
			return nil, err
		}
	}
	// Cannot trip while archiveFraming covers the framing; it keeps the
	// promise explicit should that ever change.
	if err := checkArchiveSize("the archive", int64(len(blob))); err != nil {
		return nil, err
	}
	return blob, nil
}

func buildZip(members []memFile, modified time.Time) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range members {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: f.name, Method: zip.Deflate, Modified: modified})
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(f.data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// outputPath resolves Out/Dir into a concrete file path.
func outputPath(o CreateOptions, k Kind) string {
	ext := extZip
	if o.Passphrase != "" {
		ext = extEnc
	}
	out := o.Out
	if out == "" {
		return generatedPath(o.Dir, k, o.Now, ext)
	}
	if st, err := os.Stat(out); (err == nil && st.IsDir()) || strings.HasSuffix(out, "/") || strings.HasSuffix(out, string(os.PathSeparator)) {
		return generatedPath(out, k, o.Now, ext)
	}
	return out
}

// generatedPath names an archive by kind and UTC second, moving one second on
// while the name is taken, so two archives made in the same second (a manual
// one beside a scheduled one, two restores in a row) never overwrite each other.
func generatedPath(dir string, k Kind, now time.Time, ext string) string {
	for {
		p := filepath.Join(dir, k.prefix()+now.Format(stampLayout)+ext)
		if _, err := os.Lstat(p); err != nil {
			return p // free, or unreadable: the write reports the real problem
		}
		now = now.Add(time.Second)
	}
}

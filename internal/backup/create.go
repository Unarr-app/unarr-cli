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
	// KindPreRestore is the undo button for a restore; never deleted automatically.
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

	files, err := collect(o.Layout, secrets == SecretsRedacted)
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
	blob, err := buildZip(m, files)
	if err != nil {
		return Result{}, err
	}
	if o.Passphrase != "" {
		if blob, err = encrypt(blob, o.Passphrase); err != nil {
			return Result{}, err
		}
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

// collect reads the allowlisted files that exist, redacting if asked.
func collect(l Layout, redact bool) ([]memFile, error) {
	var out []memFile
	for _, s := range l.sources() {
		data, err := os.ReadFile(s.path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", s.path, err)
		}
		if redact {
			if data, err = s.redact(data); err != nil {
				return nil, fmt.Errorf("redact %s: %w", s.name, err)
			}
		}
		out = append(out, memFile{s.name, data})
	}
	return out, nil
}

func buildZip(m Manifest, files []memFile) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	mj, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	for _, f := range append([]memFile{{ManifestName, mj}}, files...) {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: f.name, Method: zip.Deflate, Modified: m.CreatedAt})
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

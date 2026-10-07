package remotefs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
	"golang.org/x/text/unicode/norm"
)

// Record contains durable metadata, not short-lived CDN URLs or API tokens.
type Record struct {
	Entry
	ID            string `json:"id"`
	Link          string `json:"link,omitempty"`
	Manifest      string `json:"manifest,omitempty"`
	Fingerprint   string `json:"fingerprint,omitempty"`
	FileIndex     int    `json:"file_index,omitempty"`
	ManifestStamp string `json:"manifest_stamp,omitempty"`
}

type Source interface {
	Name() string
	// Identity changes on account or source-directory changes, isolating caches.
	Identity() string
	List(context.Context, []Record) ([]Record, error)
	Open(context.Context, Record) (io.ReadSeekCloser, error)
	Close() error
}

type Catalog struct {
	FS      *FS
	db      *bolt.DB
	mu      sync.Mutex
	sources []Source
	records map[string][]Record
}

func OpenCatalog(dir string, sources []Source) (*Catalog, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(filepath.Join(dir, "catalog.db"), 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, fmt.Errorf("remote catalog (is another mount server running?): %w", err)
	}
	c := &Catalog{FS: New(), db: db, sources: sources, records: make(map[string][]Record)}
	if err = c.load(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return c, nil
}

func (c *Catalog) load() error {
	err := c.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte("sources-v1"))
		if err != nil {
			return err
		}
		seen := make(map[string]bool)
		for _, src := range c.sources {
			if !validPath(src.Name()) || strings.Contains(src.Name(), "/") || seen[src.Name()] {
				return fmt.Errorf("invalid or duplicate source name %q", src.Name())
			}
			seen[src.Name()] = true
			data := b.Get([]byte(src.Identity()))
			if data == nil {
				continue
			}
			var records []Record
			if err := json.Unmarshal(data, &records); err != nil {
				return fmt.Errorf("invalid saved source catalog: %w", err)
			}
			c.records[src.Name()] = records
		}
		return nil
	})
	if err != nil {
		return err
	}
	return c.publish(c.records)
}

func (c *Catalog) entries(records map[string][]Record) []Entry {
	var entries []Entry
	for _, src := range c.sources {
		for _, rec := range records[src.Name()] {
			e := rec.Entry
			e.Path = src.Name() + "/" + e.Path
			e.Key = src.Identity() + ":" + e.Key
			e.Open = func(ctx context.Context) (io.ReadSeekCloser, error) { return src.Open(ctx, rec) }
			entries = append(entries, e)
		}
	}
	return entries
}
func (c *Catalog) publish(records map[string][]Record) error { return c.FS.Replace(c.entries(records)) }

// Refresh does remote work without the publication lock. Each source has one
// refresh worker, so an unavailable provider cannot block another source.
func (c *Catalog) Refresh(ctx context.Context, src Source) error {
	c.mu.Lock()
	previous := append([]Record(nil), c.records[src.Name()]...)
	c.mu.Unlock()
	records, err := src.List(ctx, previous)
	if err != nil && len(records) == 0 {
		return err
	}
	refreshErr := err
	if err != nil {
		records = mergePartial(previous, records)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.commitSource(src, records); err != nil {
		return err
	}
	return refreshErr
}

func mergePartial(previous, incoming []Record) []Record {
	byKey := make(map[string]Record, len(previous)+len(incoming))
	// Paths already include the logical account/provider namespace. A validated
	// replacement at that path supersedes an obsolete revision key; failed
	// accounts and paths with no incoming record remain available. commitSource
	// validates the entire candidate before either persistence or publication.
	replaced := make(map[string]bool, len(incoming))
	for _, r := range incoming {
		replaced[r.Path] = true
	}
	for _, r := range previous {
		if !replaced[r.Path] {
			byKey[r.Key] = r
		}
	}
	for _, r := range incoming {
		byKey[r.Key] = r
	}
	out := make([]Record, 0, len(byKey))
	for _, r := range byKey {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func (c *Catalog) commitSource(src Source, records []Record) error {
	data, err := json.Marshal(records)
	if err != nil {
		return err
	}
	unchanged := false
	if err = c.db.View(func(tx *bolt.Tx) error {
		unchanged = bytes.Equal(data, tx.Bucket([]byte("sources-v1")).Get([]byte(src.Identity())))
		return nil
	}); err != nil {
		return err
	}
	if unchanged {
		return nil
	}
	next := make(map[string][]Record, len(c.records)+1)
	for k, v := range c.records {
		next[k] = v
	}
	next[src.Name()] = records
	// Build once, validate before committing disk, then publish in O(1).
	fs := New()
	if err = fs.Replace(c.entries(next)); err != nil {
		return err
	}
	if err = c.db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte("sources-v1")).Put([]byte(src.Identity()), data) }); err != nil {
		return err
	}
	c.records = next
	c.FS.current.Store(fs.current.Load())
	return nil
}

// Run waits for its workers before returning, so Close never races a refresh.
func (c *Catalog) Run(ctx context.Context, interval time.Duration, report func(string, error)) {
	var wg sync.WaitGroup
	for _, src := range c.sources {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				// Requests have their own timeouts. A global scan timeout would
				// prevent a large cold account from ever finishing its first scan.
				err := c.Refresh(ctx, src)
				if report != nil && ctx.Err() == nil {
					report(src.Name(), err)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	wg.Wait()
}
func (c *Catalog) Close() error {
	for _, src := range c.sources {
		_ = src.Close()
	}
	return c.db.Close()
}

func digest(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// safeName yields a portable path component for FUSE, Windows and WebDAV.
func safeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 || strings.ContainsRune(`/\:<>"|?*`, r) {
			return '_'
		}
		return r
	}, norm.NFC.String(s))
	s = strings.Trim(s, ". ")
	if s == "" {
		return "unnamed"
	}
	return s
}
func releasePath(title, id, file string) string {
	parts := strings.Split(strings.Trim(strings.ReplaceAll(file, "\\", "/"), "/"), "/")
	for i := range parts {
		parts[i] = safeName(parts[i])
	}
	return path.Join(safeName(title)+" ["+safeName(id)+"]", strings.Join(parts, "/"))
}

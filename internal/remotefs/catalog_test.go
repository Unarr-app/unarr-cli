package remotefs

import (
	"context"
	"errors"
	"io"
	"testing"
)

type fakeSource struct {
	name, id string
	records  []Record
	err      error
}

func (s *fakeSource) Name() string                                     { return s.name }
func (s *fakeSource) Identity() string                                 { return s.id }
func (s *fakeSource) List(context.Context, []Record) ([]Record, error) { return s.records, s.err }
func (s *fakeSource) Open(ctx context.Context, r Record) (io.ReadSeekCloser, error) {
	return memoryEntry(r.Path, []byte("hello")).Open(ctx)
}
func (*fakeSource) Close() error { return nil }

func TestCatalogPersistenceFailureDeletionAndAccountIsolation(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	source := &fakeSource{name: "rd", id: "account1", records: []Record{{Entry: memoryEntry("release/file", []byte("hello")), ID: "release"}}}
	c, err := OpenCatalog(dir, []Source{source})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Refresh(ctx, source); err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	source.err = errors.New("offline")
	c, err = OpenCatalog(dir, []Source{source})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Refresh(ctx, source); err == nil {
		t.Fatal("expected refresh failure")
	}
	f, err := c.FS.OpenFile(ctx, "rd/release/file", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil || string(b) != "hello" {
		t.Fatal(string(b), err)
	}
	source.err = nil
	source.records = []Record{{Entry: memoryEntry("../bad", nil)}}
	if err = c.Refresh(ctx, source); err == nil {
		t.Fatal("invalid catalog accepted")
	}
	if _, err = c.FS.Stat(ctx, "rd/release/file"); err != nil {
		t.Fatal("lost prior tree", err)
	}
	_ = c.Close()
	source.id = "other-account"
	c, err = OpenCatalog(dir, []Source{source})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.FS.Stat(ctx, "rd/release/file"); err == nil {
		t.Fatal("other account inherited files")
	}
	_ = c.Close()
	source.id = "account1"
	source.records = nil
	c, err = OpenCatalog(dir, []Source{source})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.Refresh(ctx, source); err != nil {
		t.Fatal(err)
	}
	if _, err = c.FS.Stat(ctx, "rd/release/file"); err == nil {
		t.Fatal("successful deletion not reflected")
	}
}

func TestCatalogExclusiveOwnership(t *testing.T) {
	dir := t.TempDir()
	c, err := OpenCatalog(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	other, err := OpenCatalog(dir, nil)
	if err == nil {
		_ = other.Close()
		t.Fatal("two writers acquired same catalog")
	}
}

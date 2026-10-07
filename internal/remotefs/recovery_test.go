package remotefs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nntp"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nzb"
)

func TestBoundaryPartialRevisionReopen(t *testing.T) {
	partial := false
	api := &fakeMountAPI{accounts: []agent.MountAccount{{Provider: "torbox", Revision: "rev1"}, {Provider: "real-debrid", Revision: "old"}}}
	api.page = func(p, c string) (agent.MountPage, error) {
		if partial && p == "real-debrid" {
			return agent.MountPage{}, errors.New("offline")
		}
		ref := "ref1"
		if partial {
			ref = "ref2"
		}
		entries := []agent.MountEntry{{Path: "R/v.mkv", Key: "f", Reference: ref, Size: 1}}
		if partial {
			entries = append(entries, agent.MountEntry{Path: "R/new.mkv", Key: "new", Reference: "newref", Size: 1})
		}
		return agent.MountPage{Entries: entries}, nil
	}
	s := &WebSource{API: api, AccountIdentity: "test"}
	dir := t.TempDir()
	cat, err := OpenCatalog(dir, []Source{s})
	if err != nil {
		t.Fatal(err)
	}
	if err = cat.Refresh(context.Background(), s); err != nil {
		_ = cat.Close()
		t.Fatal(err)
	}
	partial = true
	api.accounts[0].Revision = "rev2"
	err = cat.Refresh(context.Background(), s)
	if err == nil || strings.Contains(err.Error(), "duplicate") {
		t.Errorf("partial scan should publish progress with source error: %v", err)
	}
	_ = cat.Close()
	cat, err = OpenCatalog(dir, []Source{&WebSource{API: api, AccountIdentity: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	want := map[string]string{"torbox/R/v.mkv": "ref2", "torbox/R/new.mkv": "newref", "real-debrid/R/v.mkv": "ref1"}
	for _, r := range cat.records["debrid"] {
		if want[r.Path] != r.Link {
			t.Errorf("saved record %s has %s", r.Path, r.Link)
		}
		delete(want, r.Path)
	}
	if len(want) != 0 {
		t.Errorf("missing durable progress: %v", want)
	}
}

type contextResolveAPI struct {
	fakeMountAPI
	resolveContext func(context.Context) (string, error)
}

func (f *contextResolveAPI) MountResolve(ctx context.Context, _ string) (string, error) {
	return f.resolveContext(ctx)
}

// Done is evaluated by Link's waiter select; it makes the coalescing latch exact.
type waitingContext struct {
	context.Context
	entered chan struct{}
}

func (c waitingContext) Done() <-chan struct{} {
	select {
	case c.entered <- struct{}{}:
	default:
	}
	return c.Context.Done()
}

func TestBoundaryWebCancelledLeader(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) { testWebCancelledLeader(t, deadline) })
	}
}

func testWebCancelledLeader(t *testing.T, deadline bool) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "v", time.Time{}, bytes.NewReader([]byte("exact")))
	}))
	defer cdn.Close()
	started := make(chan struct{})
	var calls atomic.Int32
	api := &contextResolveAPI{resolveContext: func(ctx context.Context) (string, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		}
		return cdn.URL, nil
	}}
	s := &WebSource{API: api}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if deadline {
		cancel()
		ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
	}
	rec := Record{Entry: Entry{Size: 5}, Link: "ref"}
	leader, _ := s.Open(ctx, rec)
	defer leader.Close()
	a := make(chan error, 1)
	go func() { _, err := io.ReadAll(leader); a <- err }()
	<-started
	entered := make(chan struct{}, 1)
	waiter, _ := s.Open(waitingContext{context.Background(), entered}, rec)
	defer waiter.Close()
	b := make(chan error, 1)
	go func() {
		data, err := io.ReadAll(waiter)
		if err == nil && string(data) != "exact" {
			err = errors.New("wrong bytes")
		}
		b <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("waiter did not coalesce")
	}
	want := context.Canceled
	if deadline {
		want = context.DeadlineExceeded
	} else {
		cancel()
	}
	if err := <-a; !errors.Is(err, want) {
		t.Errorf("leader cancellation identity lost: %v", err)
	}
	if err := <-b; err != nil {
		t.Errorf("live waiter failed: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("resolver calls=%d, want two", calls.Load())
	}
}

func TestBoundaryResolutionErrorRedaction(t *testing.T) {
	for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded, errors.New("private-provider-detail")} {
		api := &contextResolveAPI{resolveContext: func(context.Context) (string, error) { return "", fmt.Errorf("secret-token-and-url: %w", sentinel) }}
		s := &WebSource{API: api}
		r, _ := s.Open(context.Background(), Record{Entry: Entry{Size: 1}, Link: "ref"})
		_, err := io.ReadAll(r)
		_ = r.Close()
		_ = s.Close()
		if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private") {
			t.Fatalf("provider detail leaked: %v", err)
		}
		if sentinel == context.Canceled || sentinel == context.DeadlineExceeded {
			if !errors.Is(err, sentinel) {
				t.Fatalf("safe sentinel lost: %v", err)
			}
		}
	}
}

func TestBoundaryNZBCollisionsRestart(t *testing.T) {
	server := nntptest.NewFakeServer(t)
	c := nntp.NewClient(server.Config())
	defer c.Close()
	dir, cache := t.TempDir(), t.TempDir()
	n := &nzb.NZB{}
	names := []string{"a:b.mkv", "a_b.mkv", "é.mkv", "e\u0301.mkv", "folder", "folder/member.mkv", "CASE.mkv", "case.mkv"}
	for i, name := range names {
		file, articles := nntptest.BuildDirectFile(name, []byte{byte('A' + i)}, 32)
		// BuildDirectFile sanitizes IDs too; give each fixture its own wire ID
		// so filename collisions cannot overwrite the fixture's article data.
		for j := range file.Files[0].Segments {
			seg := &file.Files[0].Segments[j]
			body := articles[seg.MessageID]
			delete(articles, seg.MessageID)
			seg.MessageID = fmt.Sprintf("collision-%d-%d@test", i, j)
			articles[seg.MessageID] = body
		}
		server.AddArticles(articles)
		n.Files = append(n.Files, file.Files...)
	}
	writeNZB(t, dir, "collision.nzb", n)
	healthy, articles := nntptest.BuildDirectFile("healthy.mkv", []byte("healthy"), 32)
	server.AddArticles(articles)
	writeNZB(t, dir, "healthy.nzb", healthy)
	s := &NZBSource{Directory: dir, Fetcher: c}
	cat, err := OpenCatalog(cache, []Source{s})
	if err != nil {
		t.Fatal(err)
	}
	if err = cat.Refresh(context.Background(), s); err != nil {
		_ = cat.Close()
		t.Fatal(err)
	}
	records := append([]Record(nil), cat.records["usenet"]...)
	if len(records) != len(names)+1 {
		t.Errorf("published %d records, want %d", len(records), len(names)+1)
	}
	for _, r := range records {
		file, err := cat.FS.OpenFile(context.Background(), "usenet/"+r.Path, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(file)
		_ = file.Close()
		want := "healthy"
		if r.Manifest == "collision.nzb" {
			want = string(byte('A' + r.FileIndex))
		}
		if err != nil || string(data) != want {
			t.Errorf("%s: %q, %v; want %q", r.Path, data, err, want)
		}
	}
	_ = cat.Close()
	fresh := &NZBSource{Directory: dir, Fetcher: c}
	cat, err = OpenCatalog(cache, []Source{fresh})
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	if err = cat.Refresh(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	next := cat.records["usenet"]
	if len(next) != len(records) {
		t.Fatal("restart changed file count")
	}
	for i := range records {
		if records[i].Path != next[i].Path {
			t.Errorf("restart renamed %s to %s", records[i].Path, next[i].Path)
		}
		file, err := cat.FS.OpenFile(context.Background(), "usenet/"+next[i].Path, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(file)
		_ = file.Close()
		want := "healthy"
		if next[i].Manifest == "collision.nzb" {
			want = string(byte('A' + next[i].FileIndex))
		}
		if err != nil || string(data) != want {
			t.Fatalf("reopened %s read %q: %v", next[i].Path, data, err)
		}
	}
}

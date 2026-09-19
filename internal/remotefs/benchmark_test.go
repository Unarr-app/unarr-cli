package remotefs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntp"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
)

func BenchmarkNZBHotTail(b *testing.B) {
	data := bytes.Repeat([]byte("0123456789abcdef"), 1<<18)
	n, articles := nntptest.BuildDirectFile("video.mkv", data, 64<<10)
	server := nntptest.NewFakeServer(b)
	server.AddArticles(articles)
	client := nntp.NewClient(server.Config())
	defer client.Close()
	dir := b.TempDir()
	writeNZB(b, dir, "movie.nzb", n)
	source := &NZBSource{Directory: dir, Fetcher: client}
	defer source.Close()
	records, err := source.List(context.Background(), nil)
	if err != nil || len(records) != 1 {
		b.Fatal(records, err)
	}
	read := func() {
		r, err := source.Open(context.Background(), records[0])
		if err != nil {
			b.Fatal(err)
		}
		defer r.Close()
		_, err = r.Seek(-4096, io.SeekEnd)
		if err != nil {
			b.Fatal(err)
		}
		got, err := io.ReadAll(r)
		if err != nil || !bytes.Equal(got, data[len(data)-4096:]) {
			b.Fatal("tail read", err)
		}
	}
	read()
	before := server.BodyCalls()
	b.ReportAllocs()
	b.SetBytes(4096)
	for b.Loop() {
		read()
	}
	b.ReportMetric(float64(server.BodyCalls()-before)/float64(b.N), "articles/op")
}

func BenchmarkSnapshotBuild(b *testing.B) {
	for _, size := range []int{1000, 100000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			entries := make([]Entry, size)
			for i := range entries {
				entries[i] = memoryEntry(fmt.Sprintf("rd/release-%d/movie.mkv", i), nil)
			}
			f := New()
			b.ReportAllocs()
			for b.Loop() {
				mustReplace(b, f, entries)
			}
		})
	}
}

func TestFailedRefreshAddsProgressWithoutRemovingOldFiles(t *testing.T) {
	ctx := context.Background()
	source := &fakeSource{name: "rd", id: "account", records: []Record{{Entry: memoryEntry("old", []byte("hello"))}}}
	c, err := OpenCatalog(t.TempDir(), []Source{source})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.Refresh(ctx, source); err != nil {
		t.Fatal(err)
	}
	source.records = []Record{{Entry: memoryEntry("new", []byte("hello"))}}
	source.err = context.DeadlineExceeded
	if err = c.Refresh(ctx, source); err == nil {
		t.Fatal("partial refresh must report failure")
	}
	for _, p := range []string{"rd/new", "rd/old"} {
		if _, err := c.FS.Stat(ctx, p); err != nil {
			t.Fatal(p, err)
		}
	}
	before := c.FS.current.Load()
	source.records = c.records["rd"]
	source.err = nil
	if err = c.Refresh(ctx, source); err != nil {
		t.Fatal(err)
	}
	if c.FS.current.Load() != before {
		t.Fatal("unchanged refresh rebuilt the entire tree")
	}
}

func TestLinkSameURLRenewalIsBounded(t *testing.T) {
	count := 0
	l := &Link{url: "http://cdn/file", Resolve: func(context.Context) (string, error) { count++; return "http://cdn/file", nil }}
	for i := 0; i < 100; i++ {
		_, err := l.get(context.Background(), "http://cdn/file")
		if err != nil {
			t.Fatal(err)
		}
	}
	if count != 1 {
		t.Fatal("same URL caused renewal storm", count)
	}
	l.mu.Lock()
	l.renewedAt = time.Time{}
	l.mu.Unlock()
	_, _ = l.get(context.Background(), "http://cdn/file")
	if count != 2 {
		t.Fatal("renewal never retried")
	}
}

package remotefs

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntp"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nzb"
)

func writeNZB(t testing.TB, dir, name string, n *nzb.NZB) {
	t.Helper()
	var b bytes.Buffer
	b.WriteString(`<nzb><head>`)
	if n.Password != "" {
		b.WriteString(`<meta type="password">`)
		_ = xml.EscapeText(&b, []byte(n.Password))
		b.WriteString(`</meta>`)
	}
	b.WriteString(`</head>`)
	for _, f := range n.Files {
		b.WriteString(`<file subject="`)
		_ = xml.EscapeText(&b, []byte(f.Subject))
		b.WriteString(`"><groups><group>alt.test</group></groups><segments>`)
		for _, seg := range f.Segments {
			fmt.Fprintf(&b, `<segment bytes="%d" number="%d">`, seg.Bytes, seg.Number)
			_ = xml.EscapeText(&b, []byte(seg.MessageID))
			b.WriteString(`</segment>`)
		}
		b.WriteString(`</segments></file>`)
	}
	b.WriteString(`</nzb>`)
	if err := os.WriteFile(filepath.Join(dir, name), b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNZBDirectRARAndRestart(t *testing.T) {
	for _, rar := range []bool{false, true} {
		t.Run(fmt.Sprintf("rar=%v", rar), func(t *testing.T) {
			data := bytes.Repeat([]byte("0123456789abcdef"), 8192)
			var n *nzb.NZB
			var articles map[string][]byte
			if rar {
				n, articles = nntptest.BuildRarStore("video.mkv", data, 32768, 8192)
			} else {
				n, articles = nntptest.BuildDirectFile("video.mkv", data, 8192)
			}
			server := nntptest.NewFakeServer(t)
			server.AddArticles(articles)
			client := nntp.NewClient(server.Config())
			defer client.Close()
			dir := t.TempDir()
			writeNZB(t, dir, "release.nzb", n)
			source := &NZBSource{Directory: dir, Fetcher: client}
			defer source.Close()
			records, err := source.List(context.Background(), nil)
			if err != nil || len(records) != 1 {
				t.Fatal(records, err)
			}
			calls := server.BodyCalls()
			records, err = source.List(context.Background(), records)
			if err != nil {
				t.Fatal(err)
			}
			if server.BodyCalls() != calls {
				t.Fatal("unchanged listing fetched articles")
			}
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					r, err := source.Open(context.Background(), records[0])
					if err != nil {
						t.Error(err)
						return
					}
					defer r.Close()
					_, _ = r.Seek(int64(len(data)-9000), io.SeekStart)
					b, err := io.ReadAll(r)
					if err != nil || !bytes.Equal(b, data[len(data)-9000:]) {
						t.Errorf("tail mismatch len=%d err=%v", len(b), err)
					}
				}()
			}
			wg.Wait()
			if len(source.plans) != 1 {
				t.Fatal("reader plans not shared")
			}
			cacheDir := t.TempDir()
			catalog, err := OpenCatalog(cacheDir, []Source{source})
			if err != nil {
				t.Fatal(err)
			}
			if err = catalog.Refresh(context.Background(), source); err != nil {
				t.Fatal(err)
			}
			_ = catalog.Close()
			fresh := &NZBSource{Directory: dir, Fetcher: client}
			catalog, err = OpenCatalog(cacheDir, []Source{fresh})
			if err != nil {
				t.Fatal(err)
			}
			defer catalog.Close()
			calls = server.BodyCalls()
			fi, err := catalog.FS.Stat(context.Background(), "usenet/"+records[0].Path)
			if err != nil || fi.Size() != int64(len(data)) {
				t.Fatal(fi, err)
			}
			if server.BodyCalls() != calls {
				t.Fatal("restoring catalog fetched NNTP")
			}
		})
	}
}

func TestNZBMultiFileAndUnsupported(t *testing.T) {
	server := nntptest.NewFakeServer(t)
	client := nntp.NewClient(server.Config())
	defer client.Close()
	n, articles := nntptest.BuildDirectFile("ep1.mkv", []byte("video1"), 4)
	server.AddArticles(articles)
	n2, articles := nntptest.BuildDirectFile("ep2.mkv", []byte("video2"), 4)
	server.AddArticles(articles)
	n.Files = append(n.Files, n2.Files...)
	n3, articles := nntptest.BuildDirectFile("ep1.srt", []byte("subtitle"), 4)
	server.AddArticles(articles)
	n.Files = append(n.Files, n3.Files...)
	dir := t.TempDir()
	writeNZB(t, dir, "season.nzb", n)
	s := &NZBSource{Directory: dir, Fetcher: client}
	defer s.Close()
	records, err := s.List(context.Background(), nil)
	if err != nil || len(records) != 3 {
		t.Fatal(records, err)
	}
	n.Password = "encrypted"
	writeNZB(t, dir, "season.nzb", n)
	next, err := s.List(context.Background(), records)
	if err != nil || len(next) != 3 {
		t.Fatal("unsupported change lost known entries", err)
	}
	if _, err = s.Open(context.Background(), records[0]); err == nil {
		t.Fatal("changed manifest served with stale metadata")
	}
	if err = os.Remove(filepath.Join(dir, "season.nzb")); err != nil {
		t.Fatal(err)
	}
	next, err = s.List(context.Background(), records)
	if err != nil || len(next) != 0 {
		t.Fatal("deletion not applied", err)
	}
}

func TestNZBMissingSidecarKeepsVideoAndRetriesUnchangedManifest(t *testing.T) {
	server := nntptest.NewFakeServer(t)
	client := nntp.NewClient(server.Config())
	defer client.Close()
	n, video := nntptest.BuildDirectFile("movie.mkv", []byte("playable video"), 32)
	server.AddArticles(video)
	sidecar, missing := nntptest.BuildDirectFile("movie.nfo", []byte("metadata"), 32)
	n.Files = append(sidecar.Files, n.Files...) // Failure before the video matters.
	dir := t.TempDir()
	writeNZB(t, dir, "release.nzb", n)
	s := &NZBSource{Directory: dir, Fetcher: client}
	defer s.Close()
	ctx := context.Background()
	partial, err := s.List(ctx, nil)
	if err != nil || len(partial) != 1 || partial[0].FileIndex != 1 {
		t.Fatalf("missing sidecar hid healthy video: %v, %v", partial, err)
	}
	r, err := s.Open(ctx, partial[0])
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || string(got) != "playable video" {
		t.Fatalf("healthy video read: %q, %v", got, err)
	}
	server.AddArticles(missing)
	beforeRecovery := server.BodyCalls()
	recovered, err := s.List(ctx, partial)
	if err != nil || len(recovered) != 2 {
		t.Fatalf("unchanged manifest did not recover: %v, %v", recovered, err)
	}
	if server.BodyCalls() != beforeRecovery+1 {
		t.Fatal("partial recovery fetched articles for an already indexed file")
	}
	calls := server.BodyCalls()
	if _, err = s.List(ctx, recovered); err != nil || server.BodyCalls() != calls {
		t.Fatal("complete manifest must resume metadata-only refreshes", err)
	}
}

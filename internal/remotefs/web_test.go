package remotefs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

type fakeMountAPI struct {
	accounts []agent.MountAccount
	page     func(string, string) (agent.MountPage, error)
	resolve  func(string) (string, error)
}

func (f *fakeMountAPI) MountAccounts(context.Context) ([]agent.MountAccount, error) {
	return f.accounts, nil
}
func (f *fakeMountAPI) MountLibrary(_ context.Context, p, rev, c string) (agent.MountPage, error) {
	return f.page(p, c)
}
func (f *fakeMountAPI) MountResolve(_ context.Context, r string) (string, error) { return f.resolve(r) }
func TestWebSourceCatalogAndLazyRanges(t *testing.T) {
	var resolves, media atomic.Int32
	data := []byte("0123456789")
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		media.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("agent key leaked to CDN")
		}
		http.ServeContent(w, r, "v.mkv", time.Time{}, bytes.NewReader(data))
	}))
	defer cdn.Close()
	api := &fakeMountAPI{accounts: []agent.MountAccount{{Provider: "torbox", Revision: "revision"}},
		page: func(p, c string) (agent.MountPage, error) {
			if c == "" {
				return agent.MountPage{Entries: []agent.MountEntry{{Path: "Release/v.mkv", Key: "file", Size: 10, Reference: "signed-ref"}}, Next: "next"}, nil
			}
			return agent.MountPage{Entries: []agent.MountEntry{}}, nil
		}, resolve: func(ref string) (string, error) {
			resolves.Add(1)
			if ref != "signed-ref" {
				t.Error(ref)
			}
			return cdn.URL, nil
		}}
	source := &WebSource{API: api, AccountIdentity: "web-account"}
	cat, err := OpenCatalog(t.TempDir(), []Source{source})
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	if err = cat.Refresh(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	file, err := cat.FS.OpenFile(context.Background(), "/debrid/torbox/Release/v.mkv", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	_, _ = file.Stat()
	_, _ = file.Seek(6, io.SeekStart)
	if resolves.Load() != 0 || media.Load() != 0 {
		t.Fatal("metadata contacted origin")
	}
	got, err := io.ReadAll(file)
	if err != nil || string(got) != "6789" {
		t.Fatal(string(got), err)
	}
	if resolves.Load() != 1 {
		t.Fatal(resolves.Load())
	}
}
func TestWebSourcePartialRefreshAndAccountRemoval(t *testing.T) {
	api := &fakeMountAPI{accounts: []agent.MountAccount{{Provider: "torbox", Revision: "r"}}, page: func(string, string) (agent.MountPage, error) {
		return agent.MountPage{Entries: []agent.MountEntry{{Path: "x/v", Key: "k", Size: 1, Reference: "ref"}}}, nil
	}}
	source := &WebSource{API: api, AccountIdentity: "a"}
	cat, err := OpenCatalog(t.TempDir(), []Source{source})
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	if err = cat.Refresh(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	api.page = func(string, string) (agent.MountPage, error) { return agent.MountPage{}, errors.New("secret-token") }
	if err = cat.Refresh(context.Background(), source); err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatal(err)
	}
	if _, err = cat.FS.Stat(context.Background(), "/debrid/torbox/x/v"); err != nil {
		t.Fatal(err)
	}
	api.accounts = []agent.MountAccount{}
	if err = cat.Refresh(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	if _, err = cat.FS.Stat(context.Background(), "/debrid/torbox/x/v"); err == nil {
		t.Fatal("removed web account retained")
	}
}

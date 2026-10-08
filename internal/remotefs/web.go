package remotefs

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

type MountAPI interface {
	MountAccounts(context.Context) ([]agent.MountAccount, error)
	MountLibrary(context.Context, string, string, string) (agent.MountPage, error)
	MountResolve(context.Context, string) (string, error)
}
type WebSource struct {
	API             MountAPI
	AccountIdentity string
	mu              sync.Mutex
	links           map[string]*Link
	media           *http.Client
}

func (*WebSource) Name() string       { return "debrid" }
func (s *WebSource) Identity() string { return digest("web:" + s.AccountIdentity) }
func (s *WebSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.media != nil {
		s.media.CloseIdleConnections()
	}
	return nil
}
func (s *WebSource) List(ctx context.Context, _ []Record) ([]Record, error) {
	accounts, err := s.API.MountAccounts(ctx)
	if err != nil {
		return nil, errors.New("cannot read web accounts; check your Unarr login and server version")
	}
	if accounts == nil {
		return nil, errors.New("web returned incomplete accounts")
	}
	var records []Record
	var failures []error
	for _, a := range accounts {
		if !validPath(a.Provider) || strings.Contains(a.Provider, "/") || a.Revision == "" {
			return nil, errors.New("invalid web account metadata")
		}
		items, err := s.listAccount(ctx, a)
		records = append(records, items...)
		if err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) == 0 {
		s.pruneLinks(records)
	}
	return records, errors.Join(failures...)
}
func (s *WebSource) listAccount(ctx context.Context, a agent.MountAccount) ([]Record, error) {
	var records []Record
	cursor := ""
	seen := make(map[string]bool)
	for {
		page, err := s.API.MountLibrary(ctx, a.Provider, a.Revision, cursor)
		if err != nil {
			return records, errors.New("web library refresh failed; retaining known files")
		}
		if page.Entries == nil {
			return records, errors.New("web library returned incomplete metadata")
		}
		for _, e := range page.Entries {
			if !validPath(e.Path) || e.Key == "" || e.Reference == "" || e.Size < 0 {
				return records, errors.New("invalid web library file")
			}
			records = append(records, Record{Entry: Entry{Path: a.Provider + "/" + e.Path, Key: a.Revision + ":" + e.Key, Size: e.Size}, ID: a.Provider, Link: e.Reference})
		}
		if page.Next == "" {
			return records, nil
		}
		if seen[page.Next] {
			return records, errors.New("web library repeated a pagination cursor")
		}
		seen[page.Next] = true
		cursor = page.Next
	}
}
func (s *WebSource) pruneLinks(records []Record) {
	active := make(map[string]bool, len(records))
	for _, r := range records {
		active[r.Link] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.links {
		if !active[k] {
			delete(s.links, k)
		}
	}
}
func (s *WebSource) Open(ctx context.Context, r Record) (io.ReadSeekCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.links == nil {
		s.links = make(map[string]*Link)
		s.media = HTTPClient()
	}
	l := s.links[r.Link]
	if l == nil {
		l = &Link{Resolve: func(c context.Context) (string, error) {
			u, err := s.API.MountResolve(c, r.Link)
			if err != nil {
				return "", safeResolutionError(c, err)
			}
			return u, nil
		}}
		s.links[r.Link] = l
	}
	return NewHTTPReader(ctx, s.media, l, r.Size), nil
}

func safeResolutionError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return errors.New("web file resolution failed")
}

func previousReleases(records []Record) map[string][]Record {
	byID := make(map[string][]Record)
	for _, r := range records {
		byID[r.ID] = append(byID[r.ID], r)
	}
	return byID
}

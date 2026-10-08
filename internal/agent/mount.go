package agent

import (
	"context"
	"errors"
	"net/url"
)

func (c *Client) MountAccess(ctx context.Context) error {
	var out struct {
		Allowed bool `json:"allowed"`
	}
	if err := c.doGetWith(ctx, c.mountAccessClient, "/api/internal/agent/mount/access", &out); err != nil {
		return err
	}
	if !out.Allowed {
		return errors.New("remote mount requires a paid plan; visit https://unarr.app/pricing")
	}
	return nil
}

func (c *Client) MountUsenetCredentials(ctx context.Context) (*UsenetCredentials, error) {
	var out UsenetCredentials
	if err := c.doGet(ctx, "/api/internal/agent/mount/usenet-credentials", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type MountAccount struct {
	Provider string `json:"provider"`
	Revision string `json:"revision"`
}
type MountEntry struct {
	Path      string `json:"path"`
	Key       string `json:"key"`
	Size      int64  `json:"size"`
	Reference string `json:"reference"`
}
type MountPage struct {
	Entries []MountEntry `json:"entries"`
	Next    string       `json:"next"`
}

func (c *Client) MountAccounts(ctx context.Context) ([]MountAccount, error) {
	var out struct {
		Accounts []MountAccount `json:"accounts"`
	}
	err := c.doGet(ctx, "/api/internal/agent/mount/accounts", &out)
	return out.Accounts, err
}
func (c *Client) MountLibrary(ctx context.Context, provider, revision, cursor string) (MountPage, error) {
	var out MountPage
	q := url.Values{"provider": {provider}, "revision": {revision}, "cursor": {cursor}}
	err := c.doGet(ctx, "/api/internal/agent/mount/library?"+q.Encode(), &out)
	return out, err
}
func (c *Client) MountResolve(ctx context.Context, reference string) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	err := c.doPost(ctx, "/api/internal/agent/mount/resolve", map[string]string{"reference": reference}, &out)
	return out.URL, err
}

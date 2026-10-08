package cmd

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nntp"
)

// Credentials are fetched through the existing authenticated endpoint and only
// held in memory. Neither startup nor an offline metadata listing requests them.
type mountNNTP struct {
	api         *agent.Client
	mu          sync.Mutex
	client      *nntp.Client
	credentials agent.UsenetCredentials
	refreshed   time.Time
	closed      bool
}

func (n *mountNNTP) connection(ctx context.Context) (*nntp.Client, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil, errors.New("mount NNTP closed")
	}
	if n.client != nil && time.Since(n.refreshed) < 5*time.Minute {
		return n.client, nil
	}
	credentials, err := n.api.MountUsenetCredentials(ctx)
	if err != nil {
		return nil, errors.New("cannot obtain Usenet access; check the account configured on the web")
	}
	if credentials.Host == "" || credentials.Port <= 0 {
		return nil, errors.New("invalid web Usenet credentials")
	}
	if n.client == nil || n.credentials != *credentials {
		if n.client != nil {
			_ = n.client.Close()
		}
		n.client = nntp.NewClient(nntp.Config{Host: credentials.Host, Port: credentials.Port, SSL: credentials.SSL, TLSServerName: credentials.TLSServerName, Username: credentials.Username, Password: credentials.Password, MaxConnections: credentials.MaxConnections})
		n.credentials = *credentials
	}
	n.refreshed = time.Now()
	return n.client, nil
}
func (n *mountNNTP) Body(ctx context.Context, id string) ([]byte, error) {
	c, err := n.connection(ctx)
	if err != nil {
		return nil, err
	}
	return c.Body(ctx, id)
}

func (n *mountNNTP) MaxConcurrency() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.client == nil {
		return 1
	}
	return n.client.MaxConcurrency()
}
func (n *mountNNTP) BodyInto(ctx context.Context, id string, b []byte) ([]byte, error) {
	c, err := n.connection(ctx)
	if err != nil {
		return nil, err
	}
	return c.BodyInto(ctx, id, b)
}
func (n *mountNNTP) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.closed = true
	n.credentials = agent.UsenetCredentials{}
	if n.client != nil {
		return n.client.Close()
	}
	return nil
}

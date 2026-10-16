package cmd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/metrics"
)

// errNoAgentIdentity: nothing to publish for until this machine has an id.
var errNoAgentIdentity = errors.New("this machine has no agent id yet")

// webSessionPoster is the daemon's metrics.Poster: the agent client, for the
// identity in force at send time (a fresh sign-in mints a new one without a
// restart).
type webSessionPoster struct {
	client  *agent.Client
	agentID func() string
}

func (p webSessionPoster) PostSessions(ctx context.Context, snapshot []byte) error {
	id := p.agentID()
	if id == "" {
		return errNoAgentIdentity
	}
	return classifyPublishError(p.client.PublishSessions(ctx, id, snapshot))
}

// classifyPublishError maps the answers the publisher treats specially onto
// its sentinels, keeping the *HTTPError in the chain. Only the app's own
// 404/405 means "this web predates the endpoint": the edge's 404 during a
// deploy is a blip. 401/403 and the rest stay plain failures — recovering a
// rejected credential is the sync loop's job, and its fresh key is what the
// next post uses.
func classifyPublishError(err error) error {
	var he *agent.HTTPError
	if !errors.As(err, &he) || he.FromGateway {
		return err
	}
	switch he.StatusCode {
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return fmt.Errorf("%w: %w", metrics.ErrPublishUnsupported, err)
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		return fmt.Errorf("%w: %w", metrics.ErrPublishRejected, err)
	}
	return err
}

// startSessionPublisher starts sending the session view to the web unless
// [metrics] publish = false. It does not look at [metrics] enabled: the hub
// exists either way. stop ends it cleanly (a no-op when it is off).
func startSessionPublisher(ctx context.Context, hub *metrics.Hub, mc config.MetricsConfig, poster metrics.Poster) (stop func()) {
	if !mc.Publish {
		log.Printf("[sessions] not publishing to the web ([metrics] publish = false)")
		return func() {}
	}
	return metrics.NewPublisher(hub.Poll, poster).Start(ctx)
}

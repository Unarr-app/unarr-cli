package cmd

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"sync"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/doctor"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nntp"
	"github.com/anacrolix/dht/v2"
)

// Per-method connectivity (plan A1.6). doctor checked that par2 was installed
// and nothing else about downloading: whether the usenet login still works,
// whether the DHT is reachable, whether this account even has debrid. Every one
// of those surfaces today as "the download did not start", with no way to tell
// which of them it was.
//
// methodProbeTimeout bounds each probe. Ten seconds is the plan's figure and it
// is the right order: an NNTP login is one round-trip plus TLS, and a doctor
// that hangs is a doctor people stop running.
const methodProbeTimeout = 10 * time.Second

// dhtProbeTimeout is shorter. A bootstrap node either answers a ping quickly or
// is not going to; the failure being detected (UDP blocked outright) produces
// silence, so this timeout IS the result rather than an error to wait for.
const dhtProbeTimeout = 6 * time.Second

func doctorMethodSpecs(cfg *config.Config) []doctor.Spec {
	// One Register call for the whole doctor run, shared by the checks that
	// need to know what the ACCOUNT has rather than what the config asks for.
	// Built here so it lives exactly as long as this spec list.
	features := newFeatureCache(cfg)
	return []doctor.Spec{
		{
			Group:  "Methods",
			Name:   "Usenet server",
			Remedy: "check the usenet add-on at https://unarr.app, or drop usenet from preferred_methods",
			Fn:     func() (string, error) { return usenetConnectivityResult(cfg, features) },
		},
		{
			Group: "Methods",
			Name:  "Torrent network",
			Fn:    func() (string, error) { return torrentConnectivityResult(cfg, features) },
		},
		{
			Group: "Methods",
			Name:  "Debrid",
			Fn:    func() (string, error) { return debridResult(cfg, features) },
		},
	}
}

// featureFn answers "what can this account actually use?".
type featureFn func() (agent.FeatureFlags, error)

// newFeatureCache memoises one Register round-trip.
//
// The flags are needed because preferred_methods = "auto" — the DEFAULT —
// makes MethodOrder() return nil, which means "the server decides", not "no
// methods". Reading nil as "nothing enabled" would have silently skipped every
// probe on a stock install, and reading it as "probe everything" would report a
// usenet login failure to someone who never had the add-on. The account's own
// feature flags are the only thing that answers it.
func newFeatureCache(cfg *config.Config) featureFn {
	var (
		once  sync.Once
		flags agent.FeatureFlags
		err   error
	)
	return func() (agent.FeatureFlags, error) {
		once.Do(func() {
			key := effectiveAPIKey(cfg)
			if key == "" {
				err = fmt.Errorf("no API key")
				return
			}
			// An UNREGISTERED agent must not call Register here. This is a
			// diagnostic: registering as a side effect of running `unarr
			// doctor` would mint an agent record on the server for a machine
			// that has not been set up, which is a change, not a check. The
			// "Agent registration" check guards the same way and points the
			// user at `unarr doctor --fix` or `unarr init`.
			if cfg.Agent.ID == "" {
				err = fmt.Errorf("agent not registered")
				return
			}
			ac := agent.NewClient(cfg.Auth.APIURL, key, "unarr/"+Version)
			ctx, cancel := context.WithTimeout(context.Background(), methodProbeTimeout)
			defer cancel()
			var resp *agent.RegisterResponse
			resp, err = ac.Register(ctx, agent.RegisterRequest{
				AgentID: cfg.Agent.ID,
				Name:    cfg.Agent.Name,
				OS:      runtime.GOOS,
				Arch:    runtime.GOARCH,
				Version: Version,
			})
			if err == nil && resp != nil {
				flags = resp.Features
			}
		})
		return flags, err
	}
}

// methodWanted decides whether a method is worth probing: named explicitly in
// preferred_methods, or left to "auto" and available on this account.
func methodWanted(cfg *config.Config, method string, available bool) bool {
	order := cfg.Download.MethodOrder()
	if order == nil {
		return available // "auto" — the server picks from what the account has
	}
	for _, m := range order {
		if m == method {
			return true
		}
	}
	return false
}

func usenetConnectivityResult(cfg *config.Config, features featureFn) (string, error) {
	flags, ferr := features()
	if ferr != nil {
		return "!cannot tell whether usenet is enabled (" + ferr.Error() + ")", nil
	}
	if !methodWanted(cfg, "usenet", flags.Usenet) {
		return "not in use", nil
	}
	if !flags.Usenet {
		return "!usenet is in preferred_methods but this account has no usenet add-on", nil
	}

	// The agent holds no usenet credentials of its own — the server issues them
	// per session (see UsenetDownloader.getOrCreateNNTP), so this is the same
	// path a real download takes, not a parallel one that could drift.
	credCtx, cancelCreds := context.WithTimeout(context.Background(), methodProbeTimeout)
	defer cancelCreds()
	key := effectiveAPIKey(cfg)
	ac := agent.NewClient(cfg.Auth.APIURL, key, "unarr/"+Version)
	creds, err := ac.GetUsenetCredentials(credCtx)
	if err != nil {
		return "the API would not issue usenet credentials", err
	}

	// A BUDGET OF ITS OWN, not the remainder of the one above. Sharing a single
	// deadline meant a slow credential call silently ate the login's time, so a
	// healthy NNTP server would be reported as unreachable because the API took
	// eight of the ten seconds.
	loginCtx, cancelLogin := context.WithTimeout(context.Background(), methodProbeTimeout)
	defer cancelLogin()
	return probeNNTP(loginCtx, creds)
}

// probeNNTP opens ONE connection and authenticates.
//
// One, not the pool's full width: this is a login test, and opening the twenty
// connections the account allows would take a slot from a download in flight
// and could trip the provider's concurrency limit — turning the diagnostic into
// the fault. Nothing is fetched; Connect authenticates and that is the whole
// question.
func probeNNTP(ctx context.Context, creds *agent.UsenetCredentials) (string, error) {
	client := nntp.NewClient(nntp.Config{
		Host:           creds.Host,
		Port:           creds.Port,
		SSL:            creds.SSL,
		TLSServerName:  creds.TLSServerName,
		Username:       creds.Username,
		Password:       creds.Password,
		MaxConnections: 1,
	})
	if err := client.Connect(ctx); err != nil {
		return fmt.Sprintf("%s:%d — login failed", creds.Host, creds.Port), err
	}
	defer client.Close()

	transport := "plaintext"
	if creds.SSL {
		transport = "SSL"
	}
	return fmt.Sprintf("%s:%d (%s), %d connection slots", creds.Host, creds.Port, transport, creds.MaxConnections), nil
}

func torrentConnectivityResult(cfg *config.Config, features featureFn) (string, error) {
	flags, ferr := features()
	if ferr != nil {
		return "!cannot tell whether torrent is enabled (" + ferr.Error() + ")", nil
	}
	if !methodWanted(cfg, "torrent", flags.Torrent) {
		return "not in use", nil
	}

	port := portStatusForTorrent(cfg.Download.ListenPort)
	nodes, err := probeDHT()
	if err != nil {
		// WARN, not FAIL. A blocked DHT costs peer discovery from the open
		// swarm; trackers and the account's own sources still work, so plenty
		// of downloads succeed without it. Calling this red would paint doctor
		// red on every restrictive network for a degradation, not an outage.
		return fmt.Sprintf("!%s; DHT bootstrap did not answer (%v) — peer discovery will be limited", port, err), nil
	}
	return fmt.Sprintf("%s; DHT bootstrap answered (%d node(s))", port, nodes), nil
}

// portStatusForTorrent describes listen_port without ever failing on it. An
// incoming peer port that is closed costs inbound connections, not the ability
// to download — the agent still dials out — so this is context for the message
// above, not a verdict of its own.
func portStatusForTorrent(port int) string {
	if port == 0 {
		return "listen_port = 0 (random each start)"
	}
	free, err := portIsFree(port)
	switch {
	case free:
		return fmt.Sprintf("listen_port %d is free", port)
	case err != nil && !isAddrInUse(err):
		return fmt.Sprintf("listen_port %d could not be tested", port)
	default:
		return fmt.Sprintf("listen_port %d is in use (the daemon, if it is running)", port)
	}
}

// probeDHT does a real UDP round-trip to a bittorrent bootstrap node and
// returns how many answered.
//
// It binds its OWN ephemeral socket rather than listen_port: the daemon is
// usually holding that one, and a probe that fought it for the port would
// report a conflict it caused itself.
func probeDHT() (int, error) {
	conn, err := net.ListenPacket("udp", ":0")
	if err != nil {
		return 0, fmt.Errorf("no UDP socket: %w", err)
	}
	cfg := dht.NewDefaultServerConfig()
	cfg.Conn = conn
	server, err := dht.NewServer(cfg)
	if err != nil {
		conn.Close()
		return 0, err
	}

	addrs, err := dht.GlobalBootstrapAddrs("udp")
	if err != nil {
		server.Close()
		return 0, fmt.Errorf("bootstrap addresses did not resolve: %w", err)
	}

	answered, wait, err := pingBootstrapNodes(server, addrs)
	// CLOSED ONLY ONCE EVERY PING HAS RETURNED, and in the background so the
	// deadline above is still what the user waits for.
	//
	// `defer server.Close()` was wrong here. This function returns as soon as
	// the deadline fires, which leaves ping goroutines sitting inside
	// Server.Query — and Query is called with context.TODO() upstream, so
	// nothing cancels them but the library's own transaction timeout. Closing
	// underneath them means calling into a server whose socket is being closed
	// concurrently. Reading dht's Close() suggests it survives that; "suggests"
	// is not a guarantee worth taking when waiting costs nothing.
	go func() {
		wait()
		server.Close()
	}()
	return answered, err
}

// pingBootstrapNodes pings every bootstrap address at once, in parallel because
// the failure being tested for is silence: walking them one at a time would
// multiply the timeout by the number of nodes, and one answer is all the
// question needs.
//
// It returns a wait function alongside the count. The caller must not close the
// server until that returns — the goroutines outlive the deadline (see
// probeDHT). The result channel is buffered to the full width so a goroutine
// finishing after everyone stopped listening still exits instead of leaking.
func pingBootstrapNodes(server *dht.Server, addrs []dht.Addr) (int, func(), error) {
	var wg sync.WaitGroup
	ch := make(chan bool, len(addrs))
	for _, a := range addrs {
		ua, ok := a.Raw().(*net.UDPAddr)
		if !ok {
			ch <- false
			continue
		}
		wg.Add(1)
		go func(node *net.UDPAddr) {
			defer wg.Done()
			ch <- server.Ping(node).ToError() == nil
		}(ua)
	}
	wait := wg.Wait

	deadline := time.After(dhtProbeTimeout)
	answered := 0
	for range addrs {
		select {
		case ok := <-ch:
			if ok {
				answered++
			}
		case <-deadline:
			if answered > 0 {
				return answered, wait, nil
			}
			return 0, wait, fmt.Errorf("no bootstrap node answered in %s (UDP may be blocked)", dhtProbeTimeout)
		}
	}
	if answered == 0 {
		return 0, wait, fmt.Errorf("every bootstrap node refused or timed out")
	}
	return answered, wait, nil
}

// debridResult is the one method with nothing local to probe: the agent holds
// no debrid credentials, the web resolves them per download. So the useful
// question is not "can I connect" but "does this account have it at all" —
// answered by the feature flags that come back with registration.
func debridResult(cfg *config.Config, features featureFn) (string, error) {
	flags, err := features()
	if err != nil {
		return "!cannot tell whether debrid is enabled (" + err.Error() + ")", nil
	}
	if flags.Debrid {
		return "enabled on this account", nil
	}
	if methodWanted(cfg, "debrid", false) {
		return "debrid is in preferred_methods but this account has no debrid configured — " +
			"connect one at https://unarr.app", fmt.Errorf("debrid not configured")
	}
	return "not configured on this account", nil
}

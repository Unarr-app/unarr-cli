package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
)

// staticFeatures stands in for the Register round-trip.
func staticFeatures(f agent.FeatureFlags) featureFn {
	return func() (agent.FeatureFlags, error) { return f, nil }
}

func failingFeatures(msg string) featureFn {
	return func() (agent.FeatureFlags, error) { return agent.FeatureFlags{}, errors.New(msg) }
}

// The subtle one, and the reason this helper exists at all: MethodOrder()
// returns nil for preferred_methods = "auto", which is the DEFAULT. nil means
// "the server decides", not "nothing enabled" — reading it the wrong way either
// skips every probe on a stock install or reports a usenet login failure to
// someone who never had the add-on.
func TestMethodWantedTreatsAutoAsWhateverTheAccountHas(t *testing.T) {
	auto := config.Default()
	auto.Download.PreferredMethods = []string{"auto"}
	if !methodWanted(&auto, "usenet", true) {
		t.Error("auto + account has usenet: must be probed")
	}
	if methodWanted(&auto, "usenet", false) {
		t.Error("auto + account has NO usenet: must not be probed")
	}

	// An explicit list is the user's word and overrides availability, so that
	// "you asked for usenet and this account does not have it" can be said.
	explicit := config.Default()
	explicit.Download.PreferredMethods = []string{"usenet", "torrent"}
	if !methodWanted(&explicit, "usenet", false) {
		t.Error("explicitly requested usenet must be probed even when unavailable")
	}
	if methodWanted(&explicit, "debrid", true) {
		t.Error("debrid is absent from an explicit list: must not be probed")
	}

	// "auto" anywhere in the list wins — MethodOrder() returns nil for it.
	mixed := config.Default()
	mixed.Download.PreferredMethods = []string{"torrent", "auto"}
	if !methodWanted(&mixed, "usenet", true) {
		t.Error("auto anywhere in the list defers to the account")
	}
}

func TestUsenetCheckSkipsWhenNotInUse(t *testing.T) {
	cfg := config.Default()
	cfg.Download.PreferredMethods = []string{"torrent"}
	msg, err := usenetConnectivityResult(&cfg, staticFeatures(agent.FeatureFlags{Usenet: true}))
	if err != nil {
		t.Fatalf("a method not in use is not a failure: %v", err)
	}
	if !strings.Contains(msg, "not in use") {
		t.Errorf("message = %q", msg)
	}
}

// Asking for a method the account cannot serve is a real misconfiguration, but
// it is the user's own setting rather than a broken machine, so it warns.
func TestUsenetCheckWarnsWhenRequestedButUnavailable(t *testing.T) {
	cfg := config.Default()
	cfg.Download.PreferredMethods = []string{"usenet"}
	msg, err := usenetConnectivityResult(&cfg, staticFeatures(agent.FeatureFlags{Usenet: false}))
	if err != nil {
		t.Fatalf("must warn, not fail: %v", err)
	}
	if !strings.HasPrefix(msg, "!") {
		t.Errorf("expected a WARN, got %q", msg)
	}
}

// A doctor that cannot reach the API must not accuse the method of being
// broken — it does not know either way.
func TestMethodChecksWarnWhenFeaturesAreUnknown(t *testing.T) {
	cfg := config.Default()
	for name, fn := range map[string]func() (string, error){
		"usenet":  func() (string, error) { return usenetConnectivityResult(&cfg, failingFeatures("no API key")) },
		"torrent": func() (string, error) { return torrentConnectivityResult(&cfg, failingFeatures("no API key")) },
		"debrid":  func() (string, error) { return debridResult(&cfg, failingFeatures("no API key")) },
	} {
		msg, err := fn()
		if err != nil {
			t.Errorf("%s: unknown features must warn, not fail: %v", name, err)
		}
		if !strings.HasPrefix(msg, "!") {
			t.Errorf("%s: expected a WARN, got %q", name, msg)
		}
	}
}

// probeNNTP against a real (fake) NNTP server: a genuine TCP connect and a
// genuine AUTHINFO exchange, so this covers the code path a download uses
// rather than a mock of it.
func TestProbeNNTPLogsInAndReportsTheServer(t *testing.T) {
	srv := nntptest.NewFakeServer(t)
	c := srv.Config()

	msg, err := probeNNTP(context.Background(), &agent.UsenetCredentials{
		Host: c.Host, Port: c.Port, SSL: c.SSL,
		Username: c.Username, Password: c.Password,
		MaxConnections: 20,
	})
	if err != nil {
		t.Fatalf("login against the fake server failed: %v (%s)", err, msg)
	}
	if !strings.Contains(msg, c.Host) {
		t.Errorf("message does not name the server: %q", msg)
	}
	// The slot count is what tells a user whether their provider limit is the
	// reason downloads are queueing.
	if !strings.Contains(msg, "20 connection slots") {
		t.Errorf("message does not report the slots: %q", msg)
	}
}

// A rejected login is the failure this check exists for — an expired or
// revoked usenet subscription — and it must be red, not a warning.
//
// The fake server answers "281 authenticated" to anything unless told
// otherwise, so an earlier version of this test passed while proving nothing.
// RequireCorrectAuth is what makes the case expressible at all.
func TestProbeNNTPFailsOnBadCredentials(t *testing.T) {
	srv := nntptest.NewFakeServer(t)
	srv.RequireCorrectAuth()
	c := srv.Config()

	msg, err := probeNNTP(context.Background(), &agent.UsenetCredentials{
		Host: c.Host, Port: c.Port, SSL: c.SSL,
		Username: c.Username, Password: "not-the-password",
		MaxConnections: 1,
	})
	if err == nil {
		t.Fatalf("a rejected login must FAIL, got %q", msg)
	}
	if !strings.Contains(msg, c.Host) {
		t.Errorf("even on failure the message must name the server: %q", msg)
	}

	// The mirror image, on the same server: with the right password the probe
	// still succeeds, so the test above is failing for the credentials and not
	// because verification broke the server outright.
	if _, err := probeNNTP(context.Background(), &agent.UsenetCredentials{
		Host: c.Host, Port: c.Port, SSL: c.SSL,
		Username: c.Username, Password: c.Password,
		MaxConnections: 1,
	}); err != nil {
		t.Errorf("the correct password must still authenticate: %v", err)
	}
}

// The other real failure: nothing listening. Wrong host, wrong port, provider
// down, egress firewall.
func TestProbeNNTPFailsWhenTheServerIsNotThere(t *testing.T) {
	msg, err := probeNNTP(context.Background(), &agent.UsenetCredentials{
		Host: "127.0.0.1", Port: freePort(t), SSL: false,
		Username: "user", Password: "pass", MaxConnections: 1,
	})
	if err == nil {
		t.Fatalf("an unreachable server must FAIL, got %q", msg)
	}
}

// listen_port being closed costs inbound peers, not the ability to download —
// the agent still dials out. It is context, never a verdict.
func TestPortStatusForTorrentNeverPanicsAndDescribesEveryCase(t *testing.T) {
	if got := portStatusForTorrent(0); !strings.Contains(got, "random") {
		t.Errorf("port 0 = %q", got)
	}
	if got := portStatusForTorrent(freePort(t)); !strings.Contains(got, "free") {
		t.Errorf("free port = %q", got)
	}
	if got := portStatusForTorrent(listenOn(t)); !strings.Contains(got, "in use") {
		t.Errorf("held port = %q", got)
	}
}

func TestDebridResult(t *testing.T) {
	cfg := config.Default()

	if msg, err := debridResult(&cfg, staticFeatures(agent.FeatureFlags{Debrid: true})); err != nil {
		t.Errorf("configured debrid must pass: %v (%s)", err, msg)
	}

	// Not configured and not asked for is the majority of installs, and is fine.
	cfg.Download.PreferredMethods = []string{"torrent"}
	msg, err := debridResult(&cfg, staticFeatures(agent.FeatureFlags{Debrid: false}))
	if err != nil {
		t.Errorf("unconfigured debrid nobody asked for must not fail: %v (%s)", err, msg)
	}

	// Asked for and absent is a genuine dead end: every debrid download fails.
	cfg.Download.PreferredMethods = []string{"debrid"}
	if msg, err := debridResult(&cfg, staticFeatures(agent.FeatureFlags{Debrid: false})); err == nil {
		t.Errorf("debrid requested but not configured must FAIL, got %q", msg)
	}
}

// probeDHT is the one part of these checks that cannot be faked: it is a real
// UDP round-trip to the public bittorrent bootstrap nodes. Behind an env var
// rather than testing.Short(), because `go test ./...` does not pass -short, so
// a Short() guard would still run this on every CI machine — and a build agent
// with UDP egress blocked would fail on its own network policy rather than on
// this code. Run it deliberately:
//
//	UNARR_TEST_NETWORK=1 go test ./internal/cmd/ -run TestProbeDHT
func TestProbeDHTReachesTheBootstrapNodes(t *testing.T) {
	if os.Getenv("UNARR_TEST_NETWORK") == "" {
		t.Skip("set UNARR_TEST_NETWORK=1 to run the live DHT probe")
	}
	n, err := probeDHT()
	if err != nil {
		t.Fatalf("no bootstrap node answered: %v", err)
	}
	if n <= 0 {
		t.Fatalf("reported success with %d nodes", n)
	}
	t.Logf("%d bootstrap node(s) answered", n)
}

// An unregistered agent must not be registered as a SIDE EFFECT of running
// doctor: that mints a server-side record for a machine nobody set up, which is
// a change, not a check.
func TestFeatureCacheRefusesToRegisterAnUnregisteredAgent(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.APIKey = "sk_live_whatever"
	cfg.Agent.ID = "" // never registered

	// No HTTP server is stubbed here on purpose: if this reached the network at
	// all the test would hang or fail on dial, so a fast "not registered" is
	// itself the evidence that no call was made.
	_, err := newFeatureCache(&cfg)()
	if err == nil {
		t.Fatal("an unregistered agent must not be registered by a diagnostic")
	}
	if !strings.Contains(err.Error(), "not registered") {
		t.Errorf("err = %v, want it to name the reason", err)
	}
}

// The cache exists so three checks share one Register round-trip instead of
// making three.
func TestFeatureCacheCallsOnce(t *testing.T) {
	calls := 0
	var once sync.Once
	fn := featureFn(func() (agent.FeatureFlags, error) {
		once.Do(func() { calls++ })
		return agent.FeatureFlags{Torrent: true}, nil
	})
	for range 3 {
		if _, err := fn(); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	}
	if calls != 1 {
		t.Errorf("memoised call ran %d times", calls)
	}
}

// The whole usenet chain, end to end, with no credentials and no network: a
// stub API issues NNTP credentials that point at the fake NNTP server, and the
// check logs in against it.
//
// The unit tests above each cover one link. This is the one that would catch
// the links being wired to each other wrongly — a changed endpoint path, a
// field that stops being populated, a context that is already expired by the
// time the login starts.
func TestUsenetCheckEndToEndThroughAStubbedAPI(t *testing.T) {
	nntpSrv := nntptest.NewFakeServer(t)
	nntpSrv.RequireCorrectAuth()
	c := nntpSrv.Config()

	var credsHits int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/agent/usenet-credentials" {
			http.NotFound(w, r)
			return
		}
		credsHits++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"host": c.Host, "port": c.Port, "ssl": false,
			"username": c.Username, "password": c.Password,
			"maxConnections": 8,
		})
	}))
	t.Cleanup(api.Close)

	cfg := config.Default()
	cfg.Auth.APIURL = api.URL
	cfg.Auth.APIKey = "sk_live_stub"
	cfg.Download.PreferredMethods = []string{"usenet"}

	msg, err := usenetConnectivityResult(&cfg, staticFeatures(agent.FeatureFlags{Usenet: true}))
	if err != nil {
		t.Fatalf("the full chain must succeed: %v (%s)", err, msg)
	}
	if credsHits != 1 {
		t.Errorf("the credentials endpoint was called %d times, want exactly 1", credsHits)
	}
	if !strings.Contains(msg, "8 connection slots") {
		t.Errorf("the slot count did not survive the round-trip: %q", msg)
	}
}

// An API that will not issue credentials is a FAILURE, not a warning: usenet is
// in preferred_methods, the account has the add-on, and downloads cannot start.
func TestUsenetCheckFailsWhenTheAPIWillNotIssueCredentials(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"subscription expired"}`, http.StatusForbidden)
	}))
	t.Cleanup(api.Close)

	cfg := config.Default()
	cfg.Auth.APIURL = api.URL
	cfg.Auth.APIKey = "sk_live_stub"
	cfg.Download.PreferredMethods = []string{"usenet"}

	msg, err := usenetConnectivityResult(&cfg, staticFeatures(agent.FeatureFlags{Usenet: true}))
	if err == nil {
		t.Fatalf("a refused credential issue must FAIL, got %q", msg)
	}
}

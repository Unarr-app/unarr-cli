package cmd

import (
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/doctor"
	"github.com/Unarr-app/unarr-cli/internal/engine"
)

// The streaming checks (plan A1.3). Playback is the feature users notice
// breaking, and until now doctor said nothing at all about the two ports it
// runs on: a stream_port already held by another program produced a daemon that
// registered, reported healthy, and could not serve a single byte.
//
// NONE OF THESE ARE Quick. A port held by a foreign process is a real failure,
// but it is not one a restart fixes — and Docker's answer to an unhealthy
// container is to restart it. Wiring this into the HEALTHCHECK would convert a
// port conflict into a container restarting every 60 seconds forever, which is
// strictly worse than the conflict. Same reason the LAN probe stays out: it
// makes a network call.
const (
	// portProbeTimeout bounds the dial against our own ports. Loopback either
	// answers immediately or is not there.
	portProbeTimeout = 2 * time.Second
	// lanProbeTimeout bounds the LAN round-trip. A local firewall DROPs rather
	// than REJECTs, so the failure this check exists to catch presents as a
	// hang, not an error — the timeout IS the finding.
	lanProbeTimeout = 5 * time.Second
)

func doctorStreamSpecs(cfg *config.Config) []doctor.Spec {
	return []doctor.Spec{
		{
			Group:  "Streaming",
			Name:   "Stream port",
			Remedy: "free the port, or set [downloads] stream_port to one that is available",
			Fn:     func() (string, error) { return streamPortResult(cfg.Download.StreamPort, "stream_port") },
		},
		{
			Group: "Streaming",
			Name:  "HTTPS stream port",
			Fn:    func() (string, error) { return httpsStreamPortResult(cfg.Download.HTTPSStreamPort) },
		},
		{
			Group:  "Streaming",
			Name:   "Reachable from the LAN",
			Remedy: "allow inbound TCP on this port in the host firewall",
			Fn:     func() (string, error) { return lanReachabilityResult(cfg.Download.StreamPort) },
		},
	}
}

// streamPortResult decides whether a port being taken is the healthy state or
// the failure.
//
// The naive check — "can I bind it?" — gets this exactly backwards on a working
// machine: a running daemon HOLDS the stream port, so a bind failure is the
// thing we want to see. What distinguishes the two is who is on the other end,
// so a refused bind is followed by asking the port whether it is ours.
func streamPortResult(port int, key string) (string, error) {
	if port == 0 {
		return fmt.Sprintf("disabled (%s = 0)", key), nil
	}
	if free, err := portIsFree(port); free {
		if daemonIsAlive() {
			// The daemon is up and is NOT holding its own stream port. Playback
			// will fail while every other check stays green, which is the
			// combination that makes this worth a warning of its own.
			return fmt.Sprintf("!port %d is free, but the daemon is running and should be serving on it "+
				"— check `unarr logs` for a bind error at startup", port), nil
		}
		return fmt.Sprintf("port %d is free (daemon not running)", port), nil
	} else if err != nil && !isAddrInUse(err) {
		// Something other than "taken" — a permission problem on a privileged
		// port, an invalid address. Report it rather than guessing.
		return fmt.Sprintf("could not test port %d: %v", port, err), err
	}
	if unarrAnswersOn(port) {
		return fmt.Sprintf("port %d is served by unarr", port), nil
	}
	return fmt.Sprintf("port %d is held by another program (%s)", port, whoHoldsHint()),
		fmt.Errorf("port in use")
}

// httpsStreamPortResult is deliberately softer than its plaintext sibling. The
// HTTPS listener only comes up once a certificate exists, so "configured but
// not listening" is the normal state on most installs and must never be a
// failure — see the https_stream_port comment in config.go.
func httpsStreamPortResult(port int) (string, error) {
	if port == 0 {
		return "disabled (https_stream_port = 0)", nil
	}
	free, err := portIsFree(port)
	switch {
	case free:
		return fmt.Sprintf("port %d free — the HTTPS listener starts once a certificate is present", port), nil
	case err != nil && !isAddrInUse(err):
		return fmt.Sprintf("could not test port %d: %v", port, err), err
	case unarrAnswersOn(port):
		// Bound and answering PLAINTEXT on the port reserved for TLS. Whoever
		// it is, it is not the agent-TLS listener.
		return fmt.Sprintf("!port %d answers unencrypted — the HTTPS listener is not what is on it (%s)",
			port, whoHoldsHint()), nil
	default:
		// Bound, and silent to plain HTTP — which is precisely how a healthy
		// TLS listener behaves, and also how an unrelated program behaves.
		//
		// THIS CHECK STOPS HERE, on purpose. Telling the two apart means
		// completing a TLS handshake with verification disabled: the agent's
		// certificate is issued for its public hostname while this dials
		// 127.0.0.1, so a verifying client fails every time even when
		// everything is right. That needs InsecureSkipVerify, gosec G402 is
		// enabled repo-wide, and neither silencing it inline nor excluding
		// G402 globally is worth buying a nicer sentence here — the exclusion
		// would apply to the real API client too.
		//
		// So it reports what it knows. An earlier version guessed instead and
		// called a healthy TLS listener "held by another program", sending the
		// user to hunt a port conflict that did not exist. Saying less is the
		// fix, not saying it louder.
		return fmt.Sprintf("port %d is in use and silent to plain HTTP — consistent with the TLS listener "+
			"(not confirmed: this check does not complete a TLS handshake)", port), nil
	}
}

// lanReachabilityResult is the check that finds a host firewall, and it is the
// reason this group exists.
//
// It probes the LAN IP, never loopback: a firewall rule blocking inbound TCP
// leaves 127.0.0.1 answering perfectly while every phone, TV and browser on the
// network times out. Loopback would have reported a healthy agent for the exact
// fault the user is calling about.
func lanReachabilityResult(port int) (string, error) {
	if port == 0 {
		return "skipped (stream_port = 0)", nil
	}
	if !daemonIsAlive() {
		return "!daemon not running — nothing to reach (start it and re-run)", nil
	}
	ip := engine.LanIP()
	if ip == "" {
		return "!no LAN address found on this host (no route out) — cannot test", nil
	}
	url := fmt.Sprintf("http://%s:%d/health", ip, port)
	client := &http.Client{Timeout: lanProbeTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Sprintf("%s did not answer — a host firewall is the usual cause (%s)", url, firewallHint()),
			fmt.Errorf("unreachable on the LAN")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("!%s answered %s", url, resp.Status), nil
	}
	return fmt.Sprintf("%s answers", url), nil
}

// portIsFree reports whether the port can be bound right now. It binds on all
// interfaces, not loopback: a daemon listening on 0.0.0.0 leaves 127.0.0.1
// bindable on some stacks, and a check that came back "free" for a port our own
// daemon is serving would be worse than no check.
func portIsFree(port int) (bool, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false, err
	}
	return true, ln.Close()
}

// isAddrInUse distinguishes "taken" from every other bind failure without
// reaching for a syscall package: the error strings differ per platform, but
// both of these appear verbatim in the message Go builds.
func isAddrInUse(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, sub := range []string{
		"address already in use",                // linux, darwin
		"Only one usage of each socket address", // windows
		"address in use",
	} {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// unarrAnswersOn asks the port whether it is one of ours. /health is the stream
// server's own endpoint (internal/engine/stream_server.go), so a 200 here is
// positive identification rather than "something is listening" — which is all a
// bare TCP connect would have proven, and is exactly the case this has to tell
// apart.
func unarrAnswersOn(port int) bool {
	return healthOK(&http.Client{Timeout: portProbeTimeout},
		fmt.Sprintf("http://127.0.0.1:%d/health", port))
}

func healthOK(client *http.Client, url string) bool {
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func daemonIsAlive() bool {
	st := agent.ReadState()
	return st != nil && isDaemonAlive(st)
}

// whoHoldsHint names the command that identifies the holding process, instead
// of identifying it here.
//
// The plan called for reading the owner PID directly. Doing that means parsing
// /proc/net/tcp and walking every /proc/*/fd to match the socket inode on
// Linux, and an iphlpapi call on Windows — a few hundred lines, running as the
// user's own uid so it cannot see a process owned by anyone else, in a
// diagnostic whose entire job is to be read by a human who has a shell open.
// The one command that does it properly is one line away.
func whoHoldsHint() string {
	if runtime.GOOS == "windows" {
		return "find it with: netstat -ano | findstr LISTENING"
	}
	return "find it with: ss -ltnp | grep :<port>"
}

func firewallHint() string {
	switch runtime.GOOS {
	case "windows":
		return "check Windows Defender Firewall inbound rules"
	case "darwin":
		return "check System Settings → Network → Firewall"
	default:
		return "check ufw/firewalld/iptables"
	}
}

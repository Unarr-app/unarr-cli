package agent

// Terminal failures: the ones a user has to resolve, told apart from the ones
// worth retrying.
//
// The daemon only ever had two categories: transient (retry with backoff) and
// everything else (return an error, exit non-zero). Under a supervisor
// "everything else" is the worst possible outcome — systemd's Restart=always
// respawns the daemon, it fails the same way, and the machine sits in a restart
// loop forever. Nothing tells the user, because the daemon's explanation goes to
// stdout and a tray user has no terminal. A rejected API key produced exactly
// that: a silent loop, and 25 crash reports until the server rate-limited them.
//
// A rejected key is not a crash. It is a question for the user. So terminal
// failures are recorded here as a state on disk — reason, what happened, what to
// do about it — which the daemon writes and then STAYS UP for, because exiting
// under a Restart=always supervisor only brings the process back to fail the
// same way. The tray reads it to show the user an action instead of a
// flickering error.

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// BlockReason identifies what is wrong, so a GUI can offer the matching action
// rather than parsing a human sentence.
type BlockReason string

const (
	// BlockSignIn: the server rejected this agent's credential. Re-authorizing
	// the machine is the fix.
	BlockSignIn BlockReason = "sign_in_required"
	// BlockRevoked: this agent was deleted from the dashboard, or its key
	// belongs to another machine. The credential is wiped; a fresh login mints
	// a new identity.
	BlockRevoked BlockReason = "agent_revoked"
	// BlockPlan: the account is already at its agent limit. Either free a slot
	// or upgrade.
	BlockPlan BlockReason = "plan_limit_reached"
	// BlockConflict: another active agent already claims this machine's
	// identity (duplicated config, restored backup).
	BlockConflict BlockReason = "identity_conflict"
)

// Blocked is the on-disk record of a terminal failure. Written by the daemon on
// the way out and read by the tray.
type Blocked struct {
	Reason BlockReason `json:"reason"`
	// Message says what happened, in the words the user should read. The
	// server's own message wins when it sends one — it knows details the client
	// does not (which plan, what limit).
	Message string `json:"message"`
	// Remedy is the single next step. One step, not a list: a user who is stuck
	// needs a direction, not options.
	Remedy string `json:"remedy"`
	// Status is the HTTP status behind this, when there was one. Kept for
	// support reports; never shown to the user.
	Status int       `json:"status,omitempty"`
	At     time.Time `json:"at"`
	// Version of the daemon that blocked, so a support report can tell an old
	// binary's known bug from a live one.
	Version string `json:"version,omitempty"`
}

// blockedFilePath sits next to the daemon state file so both surfaces share one
// data dir and one cleanup.
func blockedFilePath() string {
	return filepath.Join(filepath.Dir(StateFilePath()), "blocked.json")
}

// WriteBlocked records a terminal failure. Best-effort: failing to write it must
// not stop the daemon from exiting, and an unreadable record simply degrades the
// tray to its generic error handling.
func WriteBlocked(b *Blocked) {
	if b == nil {
		return
	}
	if b.At.IsZero() {
		b.At = time.Now()
	}
	path := blockedFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return
	}
	// Same 0o600 as the daemon state: the record can name the account's plan and
	// limits, which a co-tenant on a shared host has no business reading.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
	}
}

// ReadBlocked returns the recorded terminal failure, or nil when the agent is
// not blocked.
func ReadBlocked() *Blocked {
	data, err := os.ReadFile(blockedFilePath())
	if err != nil {
		return nil
	}
	var b Blocked
	if err := json.Unmarshal(data, &b); err != nil {
		return nil
	}
	if b.Reason == "" {
		return nil
	}
	return &b
}

// ClearBlocked removes the record. The daemon calls this the moment it registers
// successfully, so a stale block can never outlive the problem — a user who
// signs in again must not keep seeing "sign in required".
func ClearBlocked() {
	os.Remove(blockedFilePath())
}

// StatusBlocked is the daemon lifecycle state for a process that is up but
// cannot work until the user resolves something. Distinct from "running" so
// nothing mistakes a parked daemon for a working one, and distinct from absent
// so `unarr stop` can still find it.
const StatusBlocked = "blocked"

// publishBlockedState writes just enough state for the rest of the CLI to see a
// parked daemon: which process to signal, and that it is not working. It does
// not pretend to be a heartbeat — there is nothing to report until the block
// clears.
func (d *Daemon) publishBlockedState() {
	d.State.AgentID = d.cfg.AgentID
	d.State.Status = StatusBlocked
	d.State.Version = d.cfg.Version
	d.State.PID = os.Getpid()
	if d.State.StartedAt.IsZero() {
		d.State.StartedAt = time.Now()
	}
	WriteState(&d.State)
}

// ambiguousRetries is how many ordinary retries an ambiguous rejection gets
// before the daemon accepts it as real and tells the user.
const ambiguousRetries = 2

// ambiguousEnoughToRetry reports whether a terminal-looking failure deserves
// another ordinary retry first.
//
// A bare 401 is the only one: it can mean a dead credential, but equally a
// deploy blip or a load-balancer hiccup on our side (which is exactly why it
// never wipes anything — see IsRevoked). Parking on the first one would, during
// any auth wobble, simultaneously pop a critical desktop notification on every
// machine in the fleet telling perfectly fine users to sign in again. A couple
// of quick retries cost seconds and cover the blip; the explicit rejections
// (revoked, plan limit, identity conflict) are unambiguous and park at once.
func ambiguousEnoughToRetry(b *Blocked, attempt int) bool {
	return b.Reason == BlockSignIn && attempt < ambiguousRetries
}

// registerBackoff is the first retry delay when registration fails. A var so
// tests can drive the retry ladder without sleeping through it.
var registerBackoff = 5 * time.Second

// blockedRetry is how often a blocked daemon re-checks. Slow on purpose: it is
// waiting for a human, and a tight loop against a server that keeps saying no is
// what got the client rate-limited in the first place.
var blockedRetry = 60 * time.Second

// waitOutBlock parks the daemon on a terminal failure instead of exiting.
//
// Exiting is not an option here. The service supervisor is Restart=always, so
// quitting — with any exit status — brings the process straight back to fail the
// same way, and each pass would re-notify the user. Staying up costs nothing (no
// downloads are possible either way), keeps one process for the tray to talk to,
// and lets the daemon recover on its own the moment the user fixes the problem.
//
// Recovery is the point of the retry: signing in from the tray rewrites the
// credential on disk, so each attempt reloads it, and an upgraded plan or a
// freed machine slot starts working without anyone restarting anything.
func (d *Daemon) waitOutBlock(ctx context.Context, b *Blocked, req RegisterRequest) (*RegisterResponse, error) {
	WriteBlocked(b)
	// A parked daemon is still a live process holding the lock file, so it must
	// publish state like any other. Without this it is invisible to `unarr
	// stop` (which finds the PID here) while `unarr start` still refuses to run
	// because the flock is held — telling the user at once that no daemon is
	// running and that one already is.
	d.publishBlockedState()
	log.Printf("[agent] blocked: %s (%s) — %s", b.Message, b.Reason, b.Remedy)
	// A revoked agent is tombstoned server-side: that exact credential will
	// never be accepted again, so it is wiped here as it always was. Parking
	// afterwards is still right — the retry is what picks up the key a fresh
	// sign-in mints, turning a dead end into a recovery.
	if b.Reason == BlockRevoked && d.OnCredentialRejected != nil {
		d.OnCredentialRejected()
	}
	// Told once. A user who is blocked does not need the same popup every
	// minute; the tray carries the state from here on.
	if d.OnBlocked != nil {
		d.OnBlocked(b)
	}

	for {
		timer := time.NewTimer(blockedRetry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}

		if d.ReloadCredential != nil {
			// The user may have signed in meanwhile, which mints a new key on
			// disk. Without picking it up, the daemon would keep retrying the
			// rejected one forever and the sign-in would look like it failed.
			d.ReloadCredential()
		}
		// Rebuilt from d.cfg every attempt, not reused from the failed one: a
		// sign-in after a revocation mints a new AGENT ID as well as a new key,
		// and re-sending the tombstoned id would be rejected forever no matter
		// how good the credential.
		req.AgentID = d.cfg.AgentID
		resp, err := d.client.Register(ctx, req)
		if err == nil {
			log.Printf("[agent] recovered from %s — registered", b.Reason)
			ClearBlocked()
			return resp, nil
		}
		if nb, terminal := Classify(err); terminal {
			// Still blocked. Re-record only when the reason changed, so a fixed
			// sign-in that now hits a plan limit updates the tray, while the
			// same failure repeating stays quiet.
			if nb.Reason != b.Reason {
				nb.Version = b.Version
				b = nb
				WriteBlocked(b)
				log.Printf("[agent] still blocked, now: %s (%s)", b.Message, b.Reason)
				if d.OnBlocked != nil {
					d.OnBlocked(b)
				}
			}
			continue
		}
		log.Printf("[agent] blocked (%s); last attempt failed differently: %v", b.Reason, err)
	}
}

// Classify maps an error from the server to a terminal failure, or reports that
// it is not terminal. Only failures the user can act on are terminal here:
// anything else stays with the existing retry path, because declaring an
// unknown error terminal would strand a daemon that a retry would have fixed.
func Classify(err error) (*Blocked, bool) {
	if err == nil {
		return nil, false
	}
	var he *HTTPError
	if !errors.As(err, &he) {
		return nil, false
	}
	code := strings.TrimSpace(he.Message)

	switch {
	case IsRevoked(err):
		return &Blocked{
			Reason: BlockRevoked,
			Status: he.StatusCode,
			Message: serverSaid(he,
				"This machine was removed from your unarr account."),
			Remedy: "Sign in again to reconnect this machine.",
		}, true

	case he.StatusCode == http.StatusUnauthorized:
		// A bare 401 is ambiguous — it can be a deploy blip as easily as a dead
		// key — so the credential is NOT wiped here (see IsRevoked). But the
		// daemon must still stop looping and say something: the user can retry
		// or sign in, and both beat a silent restart every ten seconds.
		return &Blocked{
			Reason: BlockSignIn,
			Status: he.StatusCode,
			Message: "unarr did not accept this machine's sign-in. The saved" +
				" credential may have expired or been replaced.",
			Remedy: "Sign in again to reconnect this machine.",
		}, true

	case code == "agent_limit_reached":
		return &Blocked{
			Reason: BlockPlan,
			Status: he.StatusCode,
			Message: serverSaid(he,
				"Your plan's limit of connected machines is already in use."),
			Remedy: "Disconnect another machine, or upgrade your plan to connect this one.",
		}, true

	case code == "agent_hash_taken":
		return &Blocked{
			Reason: BlockConflict,
			Status: he.StatusCode,
			Message: serverSaid(he,
				"Another active machine is already registered with this machine's identity."),
			Remedy: "Sign in again to give this machine a new identity," +
				" or remove the duplicate from your dashboard.",
		}, true
	}
	return nil, false
}

// serverSaid prefers the server's own sentence. The server knows the specifics —
// which plan, how many machines — and its wording can be improved without
// shipping a new binary to every user. Older servers send only a machine code,
// which is not something to show a user, so those fall back to the client's
// sentence.
func serverSaid(he *HTTPError, fallback string) string {
	if d := strings.TrimSpace(he.Detail); d != "" {
		return d
	}
	return fallback
}

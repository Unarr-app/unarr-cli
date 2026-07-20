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
// do about it — which the daemon writes before exiting cleanly, and which the
// tray reads to show the user an action instead of a flickering error.

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
	// BlockConfig: the daemon cannot work with its own configuration (bad
	// api_url, unwritable download dir). Not the server's doing.
	BlockConfig BlockReason = "config_error"
)

// Blocked is the on-disk record of a terminal failure. Written by the daemon on
// the way out, read by the tray and by `unarr status`.
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

// BlockedFilePath returns the path of the terminal-failure record.
func BlockedFilePath() string { return blockedFilePath() }

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
	log.Printf("[agent] blocked: %s (%s) — %s", b.Message, b.Reason, b.Remedy)
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

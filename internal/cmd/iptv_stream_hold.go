package cmd

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/engine"
)

// iptvStreamHoldWait bounds how long a starting IPTV stream waits for a paused
// IPTV download to close its provider connection. Past it the stream opens
// anyway: a slow-to-unwind transfer must not keep the viewer from playing.
const iptvStreamHoldWait = 10 * time.Second

// iptvStreamSlotWait bounds how long a starting IPTV stream waits for the
// PREVIOUS one (preempted) to let go of the provider. Its ffmpeg is killed and
// reaped within milliseconds normally; the bound only stops a wedged teardown
// from keeping the viewer out forever.
var iptvStreamSlotWait = 10 * time.Second

// iptvStreamReleaseGrace delays giving the account back to downloads after the
// stream's Close: ffmpeg was just killed, and its socket must be gone before a
// resumed transfer asks the provider for the one connection.
var iptvStreamReleaseGrace = 2 * time.Second

// providerStreamSlot lets ONE single-connection (IPTV) stream session read the
// provider at a time, daemon-wide. Every session transition — Retry, next
// episode, quality/audio switch, takeover from another device, rescue after a
// suspended tab — closes the old session and starts the new one back to back,
// and the old teardown runs off the sync loop: without the slot the new probe
// connects while the old ffmpeg's socket is still open, and a max_connections=1
// panel refuses the newcomer or cuts both.
//
// The newest session wins, as on the web (displacement closes the older rows):
// a starter that finds the slot taken preempts the holder — again whenever the
// holder changes — then waits until the holder's Close has reaped its ffmpeg
// and released the slot. A starter that is overtaken while waiting (a third
// session arrived: double Retry, next episode twice) gives up with
// errProviderSuperseded instead of ever reading the provider.
type providerStreamSlot struct {
	sem chan struct{}

	mu     sync.Mutex
	holder string // session id holding sem; "" while free or being handed over
	seq    uint64 // arrivals so far
	newest uint64 // arrival number of the newest starter; only it may take sem

	// preempt tears the holder down (production: the web-close path, which also
	// cancels a holder that is still starting). Runs on its own goroutine.
	preempt func(sessionID string)
}

func newProviderStreamSlot(preempt func(sessionID string)) *providerStreamSlot {
	return &providerStreamSlot{sem: make(chan struct{}, 1), preempt: preempt}
}

// providerStreamSlots keys one providerStreamSlot per provider ACCOUNT, so two
// users of a shared agent watching from different IPTV accounts never preempt
// each other — only streams competing for the same account's connection do.
type providerStreamSlots struct {
	mu      sync.Mutex
	slots   map[string]*providerStreamSlot
	preempt func(sessionID string)
}

func newProviderStreamSlots(preempt func(sessionID string)) *providerStreamSlots {
	return &providerStreamSlots{slots: make(map[string]*providerStreamSlot), preempt: preempt}
}

// forURL returns the slot of the account sourceURL belongs to.
func (s *providerStreamSlots) forURL(sourceURL string) *providerStreamSlot {
	key := providerAccountKey(sourceURL)
	s.mu.Lock()
	defer s.mu.Unlock()
	slot, ok := s.slots[key]
	if !ok {
		slot = newProviderStreamSlot(s.preempt)
		s.slots[key] = slot
	}
	return slot
}

// providerAccountKey identifies the provider account a stream URL reads from:
// the host plus the Xtream user (the path segment after movie/series/live, or
// a `username` query parameter). Kept in memory only — never logged.
func providerAccountKey(sourceURL string) string {
	u, err := url.Parse(sourceURL)
	if err != nil || u.Host == "" {
		return sourceURL
	}
	host := strings.ToLower(u.Host)
	if name := u.Query().Get("username"); name != "" {
		return host + "|" + name
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) >= 3 {
		switch parts[0] {
		case "movie", "series", "live":
			return host + "|" + parts[1]
		}
	}
	if u.User != nil {
		return host + "|" + u.User.Username()
	}
	return host
}

// errProviderSuperseded: a newer session of the same provider account arrived
// while this one waited for the slot. Wraps ErrSourceUnreachable so the web
// gets source_unreachable, not a transcode fault, for the (normally already
// displaced) row.
var errProviderSuperseded = fmt.Errorf("%w: provider connection handed to a newer session", engine.ErrSourceUnreachable)

// acquire takes the slot for sessionID, preempting and waiting out a different
// holder for at most wait. The returned release is idempotent. It fails with
// errProviderSuperseded when a newer starter overtakes this one, and with ctx's
// error when the session is cancelled while waiting. When the wait runs out the
// session proceeds WITHOUT the slot — a no-op release — rather than never
// playing; that is logged, it is the invariant giving way.
func (p *providerStreamSlot) acquire(ctx context.Context, sessionID string, wait time.Duration) (release func(), err error) {
	p.mu.Lock()
	p.seq++
	me := p.seq
	p.newest = me
	p.mu.Unlock()

	timeout := time.NewTimer(wait)
	defer timeout.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	preempted := "" // holder already asked to go; a different one is asked again
	for {
		if !p.isNewest(me) {
			return nil, errProviderSuperseded
		}
		select {
		case p.sem <- struct{}{}:
			return p.take(me, sessionID)
		default:
		}
		// The holder id is published just after its send on sem, so re-read it
		// every tick; a new holder (another starter got in first) is preempted too.
		p.mu.Lock()
		holder := p.holder
		p.mu.Unlock()
		if holder != "" && holder != sessionID && holder != preempted && p.preempt != nil {
			preempted = holder
			log.Printf("[hls %s] provider busy with %s - closing the older session",
				agent.ShortID(sessionID), agent.ShortID(holder))
			go p.preempt(holder)
		}
		select {
		case p.sem <- struct{}{}:
			return p.take(me, sessionID)
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeout.C:
			log.Printf("[hls %s] provider still busy after %s - opening it anyway",
				agent.ShortID(sessionID), wait)
			return func() {}, nil
		case <-tick.C:
		}
	}
}

func (p *providerStreamSlot) isNewest(me uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.newest == me
}

// take records sessionID as the holder of the sem this caller just filled —
// unless a newer starter arrived meanwhile, in which case the sem goes straight
// back and the caller is superseded.
func (p *providerStreamSlot) take(me uint64, sessionID string) (func(), error) {
	p.mu.Lock()
	if p.newest != me {
		p.mu.Unlock()
		<-p.sem
		return nil, errProviderSuperseded
	}
	p.holder = sessionID
	p.mu.Unlock()
	return sync.OnceFunc(func() {
		p.mu.Lock()
		if p.holder == sessionID {
			p.holder = ""
		}
		p.mu.Unlock()
		<-p.sem
	}), nil
}

// holdIptvForStream is the AcquireSource of a single-connection (IPTV) stream
// session: it takes the provider slot (see providerStreamSlot) and keeps IPTV
// downloads parked from before the stream's first provider read until shortly
// after its last one — independently of the browser's lease, which can lapse
// (background tab) or be released before the teardown ends.
//
// The engine calls the release once the session's ffmpeg has been reaped, so the
// slot opens for the next stream at once; downloads get the account back only
// after iptvStreamReleaseGrace.
//
// A VLC handoff is the one case this cannot cover: the web closes the session
// (the player hands the provider URL to VLC, which connects on its own), the
// release runs, and once the browser's lease also ends — the overlay closed —
// a parked download resumes and competes with VLC for the account. The agent
// never sees VLC's connection; only the user closing VLC ends that overlap.
func holdIptvForStream(ctx context.Context, dl *engine.IptvDownloader, slot *providerStreamSlot, sessionID string) (release func(), err error) {
	// Downloads first: parking them early lets a running transfer unwind while
	// this stream waits out a previous one.
	wctx, cancel := context.WithTimeout(ctx, iptvStreamHoldWait)
	defer cancel()
	unpin := dl.HoldForStream(wctx)
	releaseSlot := func() {}
	if slot != nil {
		if releaseSlot, err = slot.acquire(ctx, sessionID, iptvStreamSlotWait); err != nil {
			// Superseded or cancelled before any read: the newer session (if
			// any) holds its own pin, so this one is given back right away.
			unpin()
			return nil, err
		}
	}
	return func() {
		releaseSlot()
		time.AfterFunc(iptvStreamReleaseGrace, unpin)
	}, nil
}

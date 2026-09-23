package cmd

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/engine"
)

// maxRecentlyClosedSessions bounds the "closed by web" memory. The web resends
// a closed id for ~35 min (max 50 per sync), so a few hundred covers many
// windows while staying a fixed, tiny footprint.
const maxRecentlyClosedSessions = 512

// recentlyClosedSessions remembers session ids the web closed, so this
// daemon never (re)starts one: a closed id can still sit in the pending list of
// the same sync, or its start goroutine can finish probing AFTER the close.
// Bounded FIFO — the oldest id is forgotten first.
type recentlyClosedSessions struct {
	mu    sync.Mutex
	max   int
	set   map[string]struct{}
	order []string
}

func newRecentlyClosedSessions(maxIDs int) *recentlyClosedSessions {
	return &recentlyClosedSessions{max: maxIDs, set: make(map[string]struct{}, maxIDs)}
}

// mark records id; reports whether it was new.
func (r *recentlyClosedSessions) mark(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.set[id]; ok {
		return false
	}
	if len(r.order) >= r.max {
		delete(r.set, r.order[0])
		r.order = r.order[1:]
	}
	r.set[id] = struct{}{}
	r.order = append(r.order, id)
	return true
}

func (r *recentlyClosedSessions) contains(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.set[id]
	return ok
}

// closedPlayerSessions is the daemon-wide "closed by web" set, checked by the
// OnStreamSession handler next to playerSessionRegistry.has.
var closedPlayerSessions = newRecentlyClosedSessions(maxRecentlyClosedSessions)

// sessionClosedByWeb reports whether the web already ended sessionID — start
// paths use it to abort instead of serving a session nobody is watching.
func sessionClosedByWeb(sessionID string) bool {
	return closedPlayerSessions.contains(sessionID)
}

// hlsSessionCloser closes one registered HLS session by id through its normal
// Close() path (ffmpeg killed, partial cache discarded, never sealed). Reports
// whether a session was registered under that id.
type hlsSessionCloser interface {
	closeSession(id string) bool
}

// hlsRegistryCloser adapts the engine HLS registry to hlsSessionCloser.
type hlsRegistryCloser struct{ reg *engine.HLSSessionRegistry }

func (h hlsRegistryCloser) closeSession(id string) bool {
	target := h.reg.Get(id)
	if target == nil {
		return false
	}
	return h.reg.CloseWhere(func(s *engine.HLSSession) bool { return s == target }) > 0
}

// webSessionCloser tears down the sessions the web reports as closed.
type webSessionCloser struct {
	registry *playerSessionRegistryT
	closed   *recentlyClosedSessions
	hls      hlsSessionCloser
	// run executes a teardown; a goroutine in production so a slow Close (tmpdir
	// removal on a NAS) never stalls the sync loop, inline in tests.
	run func(func())
}

// closeByWeb marks every id as closed (synchronously, so the pending sessions
// of the same sync already see it) and tears down the ones running here.
// Idempotent: a repeated id finds nothing left to cancel and logs nothing.
// Returns how many sessions were actually torn down.
func (c webSessionCloser) closeByWeb(ids []string) int {
	n := 0
	for _, id := range ids {
		if id == "" {
			continue
		}
		c.closed.mark(id)
		cancel, registered := c.registry.takeForClose(id)
		if !registered {
			continue
		}
		n++
		c.run(func() {
			// Close() before cancel: the HLS teardown discards the partial cache
			// explicitly instead of racing ffmpeg's ctx-kill. A /stream session's
			// cancel releases its own resources and clears the slot only while its
			// file is still the served one (slotSessionCancel).
			hlsClosed := c.hls.closeSession(id)
			cancel()
			if hlsClosed {
				log.Printf("[hls %s] closed by web: encode stopped", agent.ShortID(id))
			} else {
				log.Printf("[stream %s] closed by web: session released", agent.ShortID(id))
			}
		})
	}
	return n
}

// closePlayerSessionsByWeb is the production entry point wired to the sync's
// closedStreamSessions.
func closePlayerSessionsByWeb(ids []string, hlsReg *engine.HLSSessionRegistry) {
	webSessionCloser{
		registry: playerSessionRegistry,
		closed:   closedPlayerSessions,
		hls:      hlsRegistryCloser{reg: hlsReg},
		run:      func(f func()) { go f() },
	}.closeByWeb(ids)
}

// takeForClose removes sessionID and returns its cancel, so exactly one caller
// (a web close or a start path's own re-check) runs the teardown.
func (r *playerSessionRegistryT) takeForClose(sessionID string) (context.CancelFunc, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cancel, ok := r.cancels[sessionID]
	if !ok {
		return nil, false
	}
	delete(r.cancels, sessionID)
	return cancel, true
}

// registerServed registers cancel for a session that has just started serving,
// then re-checks the "closed by web" set. A close landing between the start
// path's last check and this registration found no entry to tear down (or took a
// placeholder), so the session is torn down here instead: mark precedes take in
// closeByWeb and add precedes the re-check here, so one side always sees the
// other. Reports whether the session is still live.
func (r *playerSessionRegistryT) registerServed(sessionID string, cancel context.CancelFunc, closed *recentlyClosedSessions) bool {
	r.add(sessionID, cancel)
	if !closed.contains(sessionID) {
		return true
	}
	if c, ok := r.takeForClose(sessionID); ok {
		c()
	}
	return false
}

// unregisterIfClosedByWeb closes a just-registered HLS session the web closed in
// the window between the start path's last check and Register: that close found
// nothing registered to stop, so without this the session would serve (and, with
// maxSessions=1, evict others) until the idle sweep. Reports whether it closed.
func unregisterIfClosedByWeb(reg *engine.HLSSessionRegistry, hsess *engine.HLSSession, sessionID string, cancel context.CancelFunc) bool {
	if !sessionClosedByWeb(sessionID) {
		return false
	}
	reg.CloseWhere(func(s *engine.HLSSession) bool { return s == hsess })
	playerSessionRegistry.remove(sessionID)
	cancel()
	return true
}

// slotClearer is the part of the /stream server a slot session's teardown
// needs (*engine.StreamServer in production).
type slotClearer interface {
	ClearFileIf(gen uint64) bool
	SlotInUse(gen uint64, recent time.Duration) bool
}

// slotReaderGrace is how recently a byte must have been served for the slot to
// count as still read; slotTeardownPoll how often a deferred teardown re-checks.
var (
	slotReaderGrace  = 5 * time.Second
	slotTeardownPoll = 2 * time.Second
)

// slotSessionCancel builds the registry cancel of a session serving the single
// /stream slot (direct-play, remux, debrid/usenet direct) with file generation
// gen. release (the session's OWN resources: usenet handle, remux ffmpeg) always
// runs, even when the session was superseded; the slot is cleared only while gen
// is still the served file, so a late teardown never cuts a newer viewer — another
// session or a VLC/task stream that took /stream since. Runs at most once.
//
// While the slot is still being read the teardown waits: the web closes the
// session when the viewer hands playback to an external player, and that player
// reads this same /stream (same generation). It runs once the readers are gone
// or another file took the slot.
func slotSessionCancel(srv slotClearer, gen uint64, release func()) context.CancelFunc {
	teardown := func() {
		if release != nil {
			release()
		}
		srv.ClearFileIf(gen)
	}
	return sync.OnceFunc(func() {
		if !srv.SlotInUse(gen, slotReaderGrace) {
			teardown()
			return
		}
		log.Printf("[stream] closed session's /stream still read (external player?) - teardown deferred")
		go func() {
			for srv.SlotInUse(gen, slotReaderGrace) {
				time.Sleep(slotTeardownPoll)
			}
			teardown()
		}()
	})
}

// serveSlotSession registers a /stream slot session right after its SetFile
// (gen) and re-checks the web close; false = closed meanwhile and torn down.
func serveSlotSession(srv slotClearer, sessionID string, gen uint64, release func()) bool {
	return playerSessionRegistry.registerServed(sessionID, slotSessionCancel(srv, gen, release), closedPlayerSessions)
}

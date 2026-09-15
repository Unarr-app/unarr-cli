package nntptest

import (
	"bufio"
	"fmt"
	"time"
)

// injectKind is a failure the fake server applies to one BODY request.
type injectKind int

const (
	injNone injectKind = iota
	injFail
	injStall
	injDelay
	injReset
)

type injection struct {
	kind  injectKind
	code  int
	delay time.Duration
}

// connState is per-connection server state a BODY handler can change.
type connState struct {
	reset bool // close with a TCP RST instead of a FIN
}

// StallNext makes the next n BODY requests go unanswered, whatever the article,
// until the client gives up: an idle pooled connection silently dropped by a NAT
// or firewall, which accepts the write and never replies.
func (s *FakeServer) StallNext(n int) {
	s.mu.Lock()
	s.stallNext = n
	s.mu.Unlock()
}

// DelayNext answers the next n BODY requests only after d: a reply that arrives
// after the client stopped waiting for it.
func (s *FakeServer) DelayNext(n int, d time.Duration) {
	s.mu.Lock()
	s.delayNext, s.delayFor = n, d
	s.mu.Unlock()
}

// ResetMidBodyNext makes the next n BODY requests send "222" and part of the
// article, then reset the connection (TCP RST): a transfer cut mid-article.
func (s *FakeServer) ResetMidBodyNext(n int) {
	s.mu.Lock()
	s.resetNext = n
	s.mu.Unlock()
}

// LimitConnections makes the server greet a new connection with "502 too many
// connections" and close it while n connections are live. A connection counts
// until the server's handler for it ends, so one the client closed while its BODY
// was stalled stays counted, like a provider that has not noticed a close yet.
func (s *FakeServer) LimitConnections(n int) {
	s.mu.Lock()
	s.maxLive = n
	s.mu.Unlock()
}

// admit returns the greeting for a new connection and whether it was admitted
// under the LimitConnections cap; an admitted connection must call leave.
func (s *FakeServer) admit() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.maxLive > 0 && s.live >= s.maxLive {
		return "502 too many connections\r\n", false
	}
	s.live++
	return "200 nntptest ready\r\n", true
}

func (s *FakeServer) leave() {
	s.mu.Lock()
	s.live--
	s.mu.Unlock()
}

// takeInjectionLocked picks what the current BODY for id suffers, consuming one
// count. Caller holds s.mu.
func (s *FakeServer) takeInjectionLocked(id string) injection {
	switch {
	case s.failCount > 0:
		s.failCount--
		return injection{kind: injFail, code: s.failCode}
	case s.stalled[id]:
		return injection{kind: injStall}
	case s.stallNext > 0:
		s.stallNext--
		return injection{kind: injStall}
	case s.delayNext > 0:
		s.delayNext--
		return injection{kind: injDelay, delay: s.delayFor}
	case s.resetNext > 0:
		s.resetNext--
		return injection{kind: injReset}
	}
	return injection{}
}

// applyInjection runs the part of an injection that happens before the answer.
// proceed=false means no answer is sent; keep says whether the connection stays open.
func (s *FakeServer) applyInjection(w *bufio.Writer, inj injection) (proceed, keep bool) {
	switch inj.kind {
	case injFail:
		if inj.code <= 0 {
			return false, false // drop connection mid-request
		}
		fmt.Fprintf(w, "%d injected failure\r\n", inj.code)
		return false, true
	case injStall:
		<-s.quit // never answer; the client must give up on its own deadline
		return false, false
	case injDelay:
		select {
		case <-time.After(inj.delay):
		case <-s.quit:
			return false, false
		}
	case injNone, injReset:
		// Answered normally; a reset is applied part-way through the body.
	}
	return true, true
}

// resetMidBody sends half of the article after the status line, gives the client
// time to consume the status line, and has the connection closed with a reset.
func (s *FakeServer) resetMidBody(w *bufio.Writer, st *connState, body []byte) bool {
	w.Write(body[:len(body)/2])
	w.Flush()
	time.Sleep(50 * time.Millisecond)
	st.reset = true
	return false
}

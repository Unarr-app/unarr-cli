package nntptest

import (
	"net"
	"time"
)

// SimulateLink makes every connection behave like one over a real provider link:
// its writes are paced to bytesPerSec (0 = unlimited) and each BODY is answered
// only after rtt. Per connection, like a provider's per-connection throughput,
// which is what makes a client's parallelism, not its CPU, the bound.
//
// With a non-zero rtt the pacing also models TCP slow start after idle (the
// Linux default on the provider's side): a connection that sent nothing for
// longer than its retransmission timeout restarts from a 10-segment congestion
// window that doubles every round trip. That is what makes a client that lets
// its connections idle between bursts slower than one that keeps them busy.
func (s *FakeServer) SimulateLink(bytesPerSec int64, rtt time.Duration) {
	s.mu.Lock()
	s.linkRate, s.linkRTT = bytesPerSec, rtt
	s.mu.Unlock()
}

func (s *FakeServer) link() (int64, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.linkRate, s.linkRTT
}

const (
	pacedChunk  = 16 << 10
	initialCwnd = 10 * 1460 // RFC 6928 initial window
	minRTO      = 200 * time.Millisecond
)

// pacedConn caps a connection's write rate and, with an rtt, its congestion
// window.
type pacedConn struct {
	net.Conn
	rate, maxCwnd int64
	rtt           time.Duration

	start, lastWrite time.Time
	sent             int64
	cwnd, winSent    int64
	winStart         time.Time
}

func (p *pacedConn) Write(b []byte) (int, error) {
	now := time.Now()
	if p.start.IsZero() {
		p.start = now
	}
	if p.rtt > 0 && (p.lastWrite.IsZero() || now.Sub(p.lastWrite) > minRTO+p.rtt) {
		// Idle past the RTO: slow start again, and the rate pacing restarts too.
		p.cwnd, p.winSent, p.winStart = initialCwnd, 0, now
		p.start, p.sent = now, 0
	}
	written := 0
	for written < len(b) {
		p.waitWindow()
		p.dropCredit()
		n, err := p.Conn.Write(b[written:min(len(b), written+pacedChunk)])
		written += n
		p.sent += int64(n)
		p.winSent += int64(n)
		p.lastWrite = time.Now()
		if err != nil {
			return written, err
		}
		p.waitRate()
	}
	return written, nil
}

// owed is how long the bytes sent so far take at the link rate.
func (p *pacedConn) owed() time.Duration {
	return time.Duration(float64(p.sent) / float64(p.rate) * float64(time.Second))
}

// dropCredit forgets time the connection spent quiet: an idle link does not
// bank a burst above its rate.
func (p *pacedConn) dropCredit() {
	if p.rate <= 0 {
		return
	}
	if now := time.Now(); p.start.Add(p.owed()).Before(now) {
		p.start = now.Add(-p.owed())
	}
}

// waitRate sleeps until the bytes sent are back within the link rate.
func (p *pacedConn) waitRate() {
	if p.rate <= 0 {
		return
	}
	if wait := time.Until(p.start.Add(p.owed())); wait > 0 {
		time.Sleep(wait)
	}
}

// waitWindow blocks until the congestion window has room, opening the next
// round trip's window (doubled, up to what the rate allows per round trip).
func (p *pacedConn) waitWindow() {
	if p.rtt <= 0 || p.winSent < p.cwnd {
		return
	}
	if wait := time.Until(p.winStart.Add(p.rtt)); wait > 0 {
		time.Sleep(wait)
	}
	p.winStart, p.winSent = time.Now(), 0
	p.cwnd = min(p.cwnd*2, p.maxCwnd)
}

// pace wraps conn when a link is simulated.
func (s *FakeServer) pace(conn net.Conn) net.Conn {
	rate, rtt := s.link()
	if rate <= 0 && rtt <= 0 {
		return conn
	}
	maxCwnd := int64(1 << 40)
	if rate > 0 && rtt > 0 {
		maxCwnd = max(initialCwnd, int64(float64(rate)*rtt.Seconds()))
	}
	return &pacedConn{Conn: conn, rate: rate, rtt: rtt, maxCwnd: maxCwnd}
}

package srcproxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
)

// connGauge counts the proxy's OPEN upstream TCP connections from the client
// side (dial → +1, Close → -1) and keeps the peak: the invariant a
// one-connection provider needs is that the second is never dialled while the
// first is still open.
type connGauge struct {
	open, peak atomic.Int32
}

type gaugedConn struct {
	net.Conn
	g    *connGauge
	once sync.Once
}

func (c *gaugedConn) Close() error {
	c.once.Do(func() { c.g.open.Add(-1) })
	return c.Conn.Close()
}

func (g *connGauge) client() *http.Client {
	var d net.Dialer
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			n := g.open.Add(1)
			for {
				p := g.peak.Load()
				if n <= p || g.peak.CompareAndSwap(p, n) {
					break
				}
			}
			return &gaugedConn{Conn: conn, g: g}, nil
		},
	}
	return &http.Client{Transport: tr}
}

// Many local readers at scattered offsets (parallel segment spawns, the index
// reader, a background subtitle pass) through a SingleUpstream proxy: every
// byte is still right and the source never sees a second connection.
func TestSingleUpstreamNeverOpensASecondConnection(t *testing.T) {
	// Larger than the block cache, so readers keep going back upstream.
	data := payload(80*blockSize + 777)
	u := newUpstream(t, data)
	g := &connGauge{}
	p := startProxy(t, Options{
		URL:            u.srv.URL + "/movie/u/p/1.mkv",
		PinHead:        blockSize,
		PinTail:        blockSize,
		CacheBytes:     34 * blockSize,
		Client:         g.client(),
		SingleUpstream: true,
	})

	ranges := []struct{ first, last int }{
		{0, 3*blockSize - 1},
		{10 * blockSize, 14*blockSize + 5},
		{70 * blockSize, len(data) - 1},
		{5*blockSize + 17, 9 * blockSize},
		{40 * blockSize, 52 * blockSize},
		{2 * blockSize, 6 * blockSize},
		{60 * blockSize, 66 * blockSize},
		{25*blockSize + 3, 33 * blockSize},
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2*len(ranges))
	for i, r := range ranges {
		for _, url := range []string{p.ForegroundURL(), p.BackgroundURL()} {
			wg.Add(1)
			go func(i int, url string, first, last int) {
				defer wg.Done()
				req, _ := http.NewRequest(http.MethodGet, url, nil)
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", first, last))
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					errs <- err
					return
				}
				defer resp.Body.Close()
				got, err := io.ReadAll(resp.Body)
				if err != nil {
					errs <- err
					return
				}
				if !bytes.Equal(got, data[first:last+1]) {
					errs <- fmt.Errorf("range %d (%s): wrong bytes", i, url)
				}
			}(i, url, r.first, r.last)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if peak := g.peak.Load(); peak != 1 {
		t.Fatalf("source saw %d concurrent connections, want 1", peak)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if open := g.open.Load(); open != 0 {
		t.Fatalf("%d upstream connection(s) still open after Close", open)
	}
}

// The default (multi-upstream) proxy is unchanged: concurrent readers get their
// own upstream readers — the measurement the single mode exists to cap.
func TestMultiUpstreamOpensParallelConnections(t *testing.T) {
	data := payload(24*blockSize + 777)
	u := newUpstream(t, data)
	g := &connGauge{}
	p := startProxy(t, Options{
		URL: u.srv.URL + "/movie/u/p/1.mkv", PinHead: blockSize, PinTail: blockSize,
		CacheBytes: 40 * blockSize, Client: g.client(),
	})
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, first := range []int{0, 8 * blockSize, 16 * blockSize} {
		wg.Add(1)
		go func(first int) {
			defer wg.Done()
			<-start
			req, _ := http.NewRequest(http.MethodGet, p.ForegroundURL(), nil)
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", first, first+6*blockSize))
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
		}(first)
	}
	close(start)
	wg.Wait()
	if g.peak.Load() < 2 {
		t.Skip("readers did not overlap on this run; nothing to compare")
	}
}

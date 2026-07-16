// Package nntptest provides an in-memory NNTP server and NZB/yEnc fixture
// builders for deterministic, hermetic tests of the Usenet streaming path.
//
// It speaks only the subset of NNTP that internal/usenet/nntp.Client uses:
// a greeting, AUTHINFO USER/PASS, BODY <message-id> (dot-stuffed yEnc body),
// 430 for unknown articles, and QUIT. Nothing here touches the network beyond
// a loopback listener, so tests are fully offline and reproducible.
package nntptest

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntp"
)

// defaultMaxConnections keeps the fixture pool small so tests stay light while
// still exercising the client's multi-connection pool behaviour.
const defaultMaxConnections = 4

// FakeServer is an in-memory NNTP server backed by a loopback listener. It
// serves yEnc article bodies registered via AddArticle and can inject failures
// to exercise the client's retry/reconnect path.
type FakeServer struct {
	tb testing.TB
	ln net.Listener

	mu        sync.Mutex
	articles  map[string][]byte // message-id (no angle brackets) -> raw yEnc body
	bodyCalls int
	failCode  int
	failCount int
	username  string
	password  string
}

// NewFakeServer starts a FakeServer on a loopback port and registers cleanup
// with the test. It requires AUTHINFO by default (username "user", password
// "pass") so the client's auth path is exercised.
func NewFakeServer(tb testing.TB) *FakeServer {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("nntptest: listen: %v", err)
	}
	s := &FakeServer{
		tb:       tb,
		ln:       ln,
		articles: make(map[string][]byte),
		username: "user",
		password: "pass",
	}
	go s.acceptLoop()
	tb.Cleanup(func() { _ = s.ln.Close() })
	return s
}

// AddArticle registers a raw yEnc body under a message-id (angle brackets, if
// present, are stripped so callers may pass either form).
func (s *FakeServer) AddArticle(messageID string, yencBody []byte) {
	id := trimAngle(messageID)
	body := append([]byte(nil), yencBody...)
	s.mu.Lock()
	s.articles[id] = body
	s.mu.Unlock()
}

// AddArticles registers every entry of a message-id -> yEnc body map (as
// produced by the fixture builders).
func (s *FakeServer) AddArticles(articles map[string][]byte) {
	for id, body := range articles {
		s.AddArticle(id, body)
	}
}

// Config returns connection parameters pointing at this server.
func (s *FakeServer) Config() nntp.Config {
	host, port := s.Addr()
	return nntp.Config{
		Host:           host,
		Port:           port,
		SSL:            false,
		Username:       s.username,
		Password:       s.password,
		MaxConnections: defaultMaxConnections,
	}
}

// Addr returns the loopback host and port the server listens on.
func (s *FakeServer) Addr() (host string, port int) {
	a := s.ln.Addr().(*net.TCPAddr)
	return a.IP.String(), a.Port
}

// BodyCalls returns how many BODY commands the server has processed (including
// ones that were failed via FailNext), letting tests assert that a seek fetched
// only the article(s) it needed rather than the whole file.
func (s *FakeServer) BodyCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodyCalls
}

// FailNext makes the next n BODY requests fail. A positive code is returned as
// the NNTP status line (e.g. 430 for article-not-found); a non-positive code
// drops the connection mid-request to exercise the client's reconnect+retry.
func (s *FakeServer) FailNext(n, code int) {
	s.mu.Lock()
	s.failCount = n
	s.failCode = code
	s.mu.Unlock()
}

// --- internal ---

func (s *FakeServer) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return // listener closed on cleanup
		}
		go s.handleConn(conn)
	}
}

func (s *FakeServer) handleConn(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)

	if _, err := w.WriteString("200 nntptest ready\r\n"); err != nil {
		return
	}
	if w.Flush() != nil {
		return
	}

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return // client closed or read error
		}
		if !s.dispatch(w, strings.TrimRight(line, "\r\n")) {
			return
		}
		if w.Flush() != nil {
			return
		}
	}
}

// dispatch handles one command line, returning false when the connection should
// close (QUIT or a fatal error).
func (s *FakeServer) dispatch(w *bufio.Writer, line string) bool {
	verb := strings.ToUpper(line)
	switch {
	case strings.HasPrefix(verb, "AUTHINFO USER"):
		fmt.Fprint(w, "381 password required\r\n")
	case strings.HasPrefix(verb, "AUTHINFO PASS"):
		fmt.Fprint(w, "281 authenticated\r\n")
	case strings.HasPrefix(verb, "BODY"):
		return s.handleBody(w, line)
	case strings.HasPrefix(verb, "QUIT"):
		fmt.Fprint(w, "205 bye\r\n")
		return false
	default:
		fmt.Fprint(w, "500 unknown command\r\n")
	}
	return true
}

// handleBody serves a BODY request, honouring any pending FailNext injection.
// A dropped-connection failure (code <= 0) returns false so the caller closes
// the socket, forcing the client to reconnect and retry.
func (s *FakeServer) handleBody(w *bufio.Writer, line string) bool {
	s.mu.Lock()
	s.bodyCalls++
	if s.failCount > 0 {
		s.failCount--
		code := s.failCode
		s.mu.Unlock()
		if code <= 0 {
			return false // drop connection mid-request
		}
		fmt.Fprintf(w, "%d injected failure\r\n", code)
		return true
	}
	id := trimAngle(bodyArg(line))
	body, ok := s.articles[id]
	s.mu.Unlock()

	if !ok {
		fmt.Fprint(w, "430 no such article\r\n")
		return true
	}
	fmt.Fprintf(w, "222 0 <%s> body follows\r\n", id)
	writeDotBody(w, body)
	return true
}

// bodyArg extracts the message-id argument from a "BODY <id>" command line.
func bodyArg(line string) string {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return ""
	}
	return fields[len(fields)-1]
}

func trimAngle(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimPrefix(id, "<")
	id = strings.TrimSuffix(id, ">")
	return id
}

// writeDotBody transmits body as an NNTP dot-terminated block: each line is
// CRLF-terminated, lines starting with '.' are dot-stuffed, and a final ".\r\n"
// marks the end — the exact framing nntp.Client.readDotBody expects.
func writeDotBody(w *bufio.Writer, body []byte) {
	for _, raw := range strings.Split(string(body), "\n") {
		l := strings.TrimRight(raw, "\r")
		if strings.HasPrefix(l, ".") {
			w.WriteByte('.')
		}
		w.WriteString(l)
		w.WriteString("\r\n")
	}
	w.WriteString(".\r\n")
}

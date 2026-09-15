package nntp

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestNewClient(t *testing.T) {
	c := NewClient(Config{Host: "news.example.com", Port: 563, SSL: true})
	if c.cfg.MaxConnections != 10 {
		t.Errorf("default MaxConnections = %d, want 10", c.cfg.MaxConnections)
	}
	if c.cfg.Host != "news.example.com" {
		t.Errorf("Host = %q", c.cfg.Host)
	}
}

func TestNewClientCustomConnections(t *testing.T) {
	c := NewClient(Config{Host: "news.example.com", Port: 563, MaxConnections: 20})
	if c.cfg.MaxConnections != 20 {
		t.Errorf("MaxConnections = %d, want 20", c.cfg.MaxConnections)
	}
}

func TestNewClientZeroConnections(t *testing.T) {
	c := NewClient(Config{Host: "news.example.com", Port: 563, MaxConnections: 0})
	if c.cfg.MaxConnections != 10 {
		t.Errorf("MaxConnections should default to 10, got %d", c.cfg.MaxConnections)
	}
}

func TestNewClientNegativeConnections(t *testing.T) {
	c := NewClient(Config{MaxConnections: -5})
	if c.cfg.MaxConnections != 10 {
		t.Errorf("MaxConnections should default to 10 for negative, got %d", c.cfg.MaxConnections)
	}
}

func TestActiveConnections(t *testing.T) {
	c := NewClient(Config{Host: "localhost", Port: 119})
	if c.ActiveConnections() != 0 {
		t.Errorf("ActiveConnections = %d, want 0", c.ActiveConnections())
	}
}

func TestStatus(t *testing.T) {
	c := NewClient(Config{Host: "news.example.com", Port: 563})
	s := c.Status()
	if s != "0 connections (0 pooled) to news.example.com:563" {
		t.Errorf("Status = %q", s)
	}
}

func TestCloseIdempotent(t *testing.T) {
	c := NewClient(Config{Host: "localhost", Port: 119})
	// Close should be idempotent
	if err := c.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestArticleNotFoundError(t *testing.T) {
	err := &ArticleNotFoundError{MessageID: "abc123@news.example.com"}
	msg := err.Error()
	if msg != "nntp: article not found: abc123@news.example.com" {
		t.Errorf("Error() = %q", msg)
	}
}

func TestReadDotBody(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			"simple body",
			"Hello World\r\n.\r\n",
			"Hello World\n",
		},
		{
			"multiline",
			"Line 1\r\nLine 2\r\nLine 3\r\n.\r\n",
			"Line 1\nLine 2\nLine 3\n",
		},
		{
			"dot-stuffed line",
			"..This starts with a dot\r\n.\r\n",
			".This starts with a dot\n",
		},
		{
			"empty body",
			".\r\n",
			"",
		},
		{
			"binary-like data",
			"=ybegin line=128 size=1024 name=test.bin\r\nsome encoded data\r\n=yend\r\n.\r\n",
			"=ybegin line=128 size=1024 name=test.bin\nsome encoded data\n=yend\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := bufio.NewReader(bytes.NewBufferString(tt.input))
			got, err := readDotBody(r, nil, nil)
			if err != nil {
				t.Fatalf("readDotBody: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("readDotBody = %q, want %q", string(got), tt.want)
			}
		})
	}
}

func TestReadDotBodyEOF(t *testing.T) {
	// No dot terminator — should read until EOF
	r := bufio.NewReader(bytes.NewBufferString("partial data\r\n"))
	got, err := readDotBody(r, nil, nil)
	if err != nil {
		t.Fatalf("readDotBody EOF: %v", err)
	}
	if string(got) != "partial data\n" {
		t.Errorf("readDotBody EOF = %q", string(got))
	}
}

// Lines longer than the reader's buffer arrive in fragments; a dot-stuffed long
// line and a partial last line must come out exactly as whole-line reads would
// give them, into the caller's buffer when it is large enough.
func TestReadDotBodyLongLinesIntoBuffer(t *testing.T) {
	long := strings.Repeat("x", 10_000)
	input := "..dotted " + long + "\r\n" + long + "\r\n\r\nshort\r\n.\r\nafter terminator\r\n"
	want := ".dotted " + long + "\n" + long + "\n\nshort\n"

	buf := make([]byte, 0, 64<<10)
	r := bufio.NewReaderSize(strings.NewReader(input), 16)
	got, err := readDotBody(r, buf, nil)
	if err != nil {
		t.Fatalf("readDotBody: %v", err)
	}
	if string(got) != want {
		t.Fatalf("readDotBody = %d bytes, want %d", len(got), len(want))
	}
	if &got[0] != &buf[:1][0] {
		t.Fatal("body was not read into the caller's buffer")
	}

	partial := bufio.NewReaderSize(strings.NewReader("whole\r\n"+long), 16)
	if got, err := readDotBody(partial, nil, nil); err != nil || string(got) != "whole\n" {
		t.Fatalf("partial last line: %q, %v; want the complete lines only", got, err)
	}
}

// progressRecorder checks each reported body and rewrites it, as an in-place
// decoder does, to prove the reader never looks at reported lines again.
type progressRecorder struct {
	t        *testing.T
	want     string
	lines    int
	restarts int
}

func (p *progressRecorder) Line(body []byte) {
	if len(body) == 0 || body[len(body)-1] != '\n' || !strings.HasPrefix(p.want, string(bytes.ToUpper(body[:len(body)-1]))+"\n") {
		p.t.Fatalf("reported body %q is not a whole-line prefix of %q", body, p.want)
	}
	p.lines++
	copy(body, bytes.ToUpper(body))
}

func (p *progressRecorder) Restart() { p.restarts++ }

// Every line is reported with the body so far, and rewriting reported bytes does
// not disturb the lines still to come, dot-unstuffing included.
func TestReadDotBodyReportsEachLine(t *testing.T) {
	long := strings.Repeat("x", 300)
	input := "line one\r\n..dotted\r\n" + long + "\r\n\r\nlast\r\n.\r\n"
	p := &progressRecorder{t: t, want: strings.ToUpper("line one\n.dotted\n" + long + "\n\nlast\n")}
	r := bufio.NewReaderSize(strings.NewReader(input), 16)
	got, err := readDotBody(r, make([]byte, 0, 8), p)
	if err != nil {
		t.Fatalf("readDotBody: %v", err)
	}
	if string(got) != p.want || p.lines != 5 {
		t.Fatalf("body %q after %d lines, want %q after 5", got, p.lines, p.want)
	}
}

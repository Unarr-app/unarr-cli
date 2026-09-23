package engine

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type memProvider struct{ data []byte }

func (p *memProvider) FileName() string { return "film.mp4" }
func (p *memProvider) FileSize() int64  { return int64(len(p.data)) }
func (p *memProvider) NewFileReader(context.Context) io.ReadSeekCloser {
	return nopSeekCloser{bytes.NewReader(p.data)}
}

type nopSeekCloser struct{ *bytes.Reader }

func (nopSeekCloser) Close() error { return nil }

// I4: an external player reading the slot keeps it "in use" (so a web close of
// the player session must not clear it); a fresh, never-read file does not.
func TestSlotInUseTracksRealReaders(t *testing.T) {
	ss := NewStreamServer(0, 1)
	ss.SetRequireStreamToken(false)
	gen := ss.SetFile(&memProvider{data: bytes.Repeat([]byte("x"), 4<<20)}, "t1")
	if ss.SlotInUse(gen, time.Second) {
		t.Fatal("a file nobody has read must not count as in use (SetFile seeds the read clock)")
	}

	ts := httptest.NewServer(http.HandlerFunc(ss.handler))
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64<<10)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	// VLC paused: the connection stays open, no bytes flow.
	if !ss.SlotInUse(gen, time.Millisecond) {
		t.Fatal("an open reader connection must keep the slot in use")
	}
	resp.Body.Close()
	deadline := time.Now().Add(3 * time.Second)
	for ss.ActiveReaders() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !ss.SlotInUse(gen, time.Minute) {
		t.Fatal("bytes served within the grace must keep the slot in use")
	}
	if ss.SlotInUse(gen, time.Nanosecond) {
		t.Fatal("no reader and no recent byte: the slot is free")
	}
	newer := ss.SetFile(&memProvider{data: []byte("y")}, "t2")
	if ss.SlotInUse(gen, time.Minute) {
		t.Fatal("a replaced generation is never in use")
	}
	_ = newer
}

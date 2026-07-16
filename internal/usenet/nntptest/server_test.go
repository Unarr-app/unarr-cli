package nntptest

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntp"
	"github.com/Unarr-app/unarr-cli/internal/usenet/yenc"
)

// dialClient connects a real nntp.Client to the fake server, failing the test
// on any connection error and registering cleanup.
func dialClient(t *testing.T, s *FakeServer) *nntp.Client {
	t.Helper()
	c := nntp.NewClient(s.Config())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestFakeServerServesArticle(t *testing.T) {
	s := NewFakeServer(t)
	payload := bytes.Repeat([]byte("hello usenet streaming\n"), 40)
	body := yenc.Encode("clip.mkv", 1, 2, 1, int64(len(payload)), int64(len(payload))*2, payload)
	s.AddArticle("art1@fake", body)

	c := dialClient(t, s)
	raw, err := c.Body(context.Background(), "art1@fake")
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	part, err := yenc.DecodeBytes(raw)
	if err != nil {
		t.Fatalf("decode served body: %v", err)
	}
	if !bytes.Equal(part.Data, payload) {
		t.Fatalf("served payload mismatch: got %d bytes", len(part.Data))
	}
	if s.BodyCalls() != 1 {
		t.Errorf("BodyCalls = %d, want 1", s.BodyCalls())
	}
}

func TestFakeServerFullByteRangeThroughDotStuffing(t *testing.T) {
	// A payload spanning every byte value forces both yEnc escapes and NNTP
	// dot-stuffing (encoded byte 0x04 -> '.'), verifying the transport framing.
	s := NewFakeServer(t)
	payload := make([]byte, 256*8)
	for i := range payload {
		payload[i] = byte(i)
	}
	body := yenc.Encode("all.bin", 1, 2, 1, int64(len(payload)), int64(len(payload))*2, payload)
	s.AddArticle("bytes@fake", body)

	c := dialClient(t, s)
	raw, err := c.Body(context.Background(), "bytes@fake")
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	part, err := yenc.DecodeBytes(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(part.Data, payload) {
		t.Fatal("full-byte-range payload corrupted through dot-stuffing/CRC")
	}
}

func TestFakeServerArticleNotFound(t *testing.T) {
	s := NewFakeServer(t)
	c := dialClient(t, s)
	_, err := c.Body(context.Background(), "missing@fake")
	var nf *nntp.ArticleNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("want ArticleNotFoundError, got %v", err)
	}
}

func TestFakeServerRetriesOnDroppedConnection(t *testing.T) {
	// A single dropped connection must be transparently retried by the client
	// (its reconnect path), so the caller still gets the article.
	s := NewFakeServer(t)
	payload := []byte("resilient payload")
	s.AddArticle("retry@fake", yenc.Encode("r.bin", 1, 1, 1, int64(len(payload)), int64(len(payload)), payload))

	c := dialClient(t, s)
	s.FailNext(1, 0) // drop the connection on the next BODY
	raw, err := c.Body(context.Background(), "retry@fake")
	if err != nil {
		t.Fatalf("Body after one dropped connection: %v", err)
	}
	part, err := yenc.DecodeBytes(raw)
	if err != nil || !bytes.Equal(part.Data, payload) {
		t.Fatalf("payload after retry: err=%v", err)
	}
	if s.BodyCalls() < 2 {
		t.Errorf("BodyCalls = %d, want >= 2 (original + retry)", s.BodyCalls())
	}
}

func TestFakeServerReuseAcrossManyArticles(t *testing.T) {
	// Many sequential fetches must reuse the connection pool without hanging,
	// and each BODY must count exactly once.
	s := NewFakeServer(t)
	const n = 20
	for i := 0; i < n; i++ {
		p := bytes.Repeat([]byte{byte(i)}, 300)
		id := "seq-" + string(rune('a'+i)) + "@fake"
		s.AddArticle(id, yenc.Encode("s.bin", 1, 1, 1, int64(len(p)), int64(len(p)), p))
	}
	c := dialClient(t, s)
	for i := 0; i < n; i++ {
		id := "seq-" + string(rune('a'+i)) + "@fake"
		if _, err := c.Body(context.Background(), id); err != nil {
			t.Fatalf("Body %s: %v", id, err)
		}
	}
	if s.BodyCalls() != n {
		t.Errorf("BodyCalls = %d, want %d", s.BodyCalls(), n)
	}
}

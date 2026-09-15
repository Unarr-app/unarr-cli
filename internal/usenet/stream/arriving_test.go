package stream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nzb"
	"github.com/Unarr-app/unarr-cli/internal/usenet/yenc"
)

// arrivalPart is large enough for pooled off-heap buffers and several publishes.
const arrivalPart = 256 << 10

// arrivingFile posts content as partSize-byte articles on s.
func arrivingFile(s *nntptest.FakeServer, content []byte, partSize int) nzb.File {
	n, articles := nntptest.BuildDirectFile("movie.mkv", content, partSize)
	s.AddArticles(articles)
	return n.Files[0]
}

// arrivingReader opens a reader over f with read-ahead off, so every article is
// fetched by the read that needs it, and with the offset map made exact the way
// http.ServeContent does before a range (Seek to the end) unless estimated.
func arrivingReader(t *testing.T, s *nntptest.FakeServer, f nzb.File, estimated bool) *Reader {
	t.Helper()
	poisonReleasedBuffers.Store(true)
	t.Cleanup(func() { poisonReleasedBuffers.Store(false) })
	r := openReader(context.Background(), readerSource{fetcher: dialFake(t, s), ix: NewOffsetIndex(f)})
	r.retryBackoff = time.Millisecond
	r.readaheadK = 0
	t.Cleanup(func() { _ = r.Close() })
	if !estimated {
		if _, err := r.Seek(0, io.SeekEnd); err != nil {
			t.Fatalf("Seek end: %v", err)
		}
	}
	return r
}

// readAllAsync reads len(p) bytes from r in the background.
func readAllAsync(r io.Reader, p []byte) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(r, p)
		done <- err
	}()
	return done
}

// checkRefsBalanced fails when a recycled part is still referenced by anything
// but the cache once every reader closed. Parts outside a pooled buffer (a body
// that outgrew it) are not counted at all.
func checkRefsBalanced(t *testing.T, r *Reader) {
	t.Helper()
	_ = r.Close()
	c := r.cache.c
	c.mu.Lock()
	defer c.mu.Unlock()
	cached := make(map[*yenc.Part]bool, c.lru.Len())
	for el := c.lru.Front(); el != nil; el = el.Next() {
		cached[el.Value.(*cacheEntry).part] = true
	}
	for part, n := range c.refs {
		if n != 1 || !cached[part] {
			t.Fatalf("a part holds %d references (cached: %v) after every reader closed", n, cached[part])
		}
	}
}

// A seek into an article still downloading is served once the bytes it asks for
// have arrived, not once the whole article has; the bytes past what arrived wait
// for the rest of the body.
func TestSeekIsServedWhileTheArticleArrives(t *testing.T) {
	content := patternBytes(8 * arrivalPart)
	s := nntptest.NewFakeServer(t)
	f := arrivingFile(s, content, arrivalPart)
	r := arrivingReader(t, s, f, false)
	release := s.HoldBody(f.Segments[3].MessageID) // only half the body arrives
	defer release()

	pos := int64(3*arrivalPart + 1000)
	if _, err := r.Seek(pos, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	first := make([]byte, 32<<10)
	select {
	case err := <-readAllAsync(r, first):
		if err != nil || !bytes.Equal(first, content[pos:pos+int64(len(first))]) {
			t.Fatalf("first read: %v, or wrong bytes", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first bytes of a seek waited for the whole article")
	}

	rest := make([]byte, arrivalPart) // runs past the half that arrived, into the next article
	done := readAllAsync(r, rest)
	select {
	case err := <-done:
		t.Fatalf("bytes the server has not sent were served (err %v)", err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	if err := <-done; err != nil {
		t.Fatalf("rest: %v", err)
	}
	from := pos + int64(len(first))
	if !bytes.Equal(rest, content[from:from+int64(len(rest))]) {
		t.Fatal("rest of the article is not the content")
	}
	checkRefsBalanced(t, r)
}

// A body larger than the NZB claims outgrows its buffer, which returns to the
// pool once the read ends: bytes decoded past the old buffer are still served as
// they arrive, and every byte is the article's.
func TestArticleOutgrowingItsBufferIsServedWhileItArrives(t *testing.T) {
	const partSize = 2 * arrivalPart
	content := patternBytes(8 * partSize)
	s := nntptest.NewFakeServer(t)
	f := arrivingFile(s, content, partSize)
	for i := range f.Segments {
		f.Segments[i].Bytes = 96 << 10 // understated alike, so the map is still pinned exactly
	}
	r := arrivingReader(t, s, f, false)
	release := s.HoldBody(f.Segments[3].MessageID) // half the body: well past the 96 KiB buffer
	defer release()

	pos := int64(3*partSize + 150<<10)
	if _, err := r.Seek(pos, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	first := make([]byte, 32<<10)
	select {
	case err := <-readAllAsync(r, first):
		if err != nil || !bytes.Equal(first, content[pos:pos+int64(len(first))]) {
			t.Fatalf("first read: %v, or wrong bytes", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bytes past the outgrown buffer waited for the whole article")
	}
	rest := make([]byte, partSize)
	done := readAllAsync(r, rest)
	time.Sleep(50 * time.Millisecond)
	release()
	if err := <-done; err != nil {
		t.Fatalf("rest: %v", err)
	}
	from := pos + int64(len(first))
	if !bytes.Equal(rest, content[from:from+int64(len(rest))]) {
		t.Fatal("article read through an outgrown buffer is not the content")
	}
	checkRefsBalanced(t, r)
}

// A body cut mid-transfer is read again over the same buffer: what the cut read
// published must be withdrawn before the retry overwrites it, and every byte the
// reader gets is the article's.
func TestCutArticleIsServedIntactAfterItsRetry(t *testing.T) {
	content := patternBytes(8 * arrivalPart)
	s := nntptest.NewFakeServer(t)
	f := arrivingFile(s, content, arrivalPart)
	r := arrivingReader(t, s, f, false)
	s.ResetMidBodyNext(1)
	release := s.HoldBody(f.Segments[3].MessageID) // holds the retry
	defer release()
	calls := s.BodyCalls()

	pos := int64(3*arrivalPart + 200<<10) // past what either read sends before the release
	if _, err := r.Seek(pos, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, arrivalPart)
	done := readAllAsync(r, got)
	time.Sleep(150 * time.Millisecond)
	release()
	if err := <-done; err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, content[pos:pos+int64(len(got))]) {
		t.Fatal("article read across a cut and its retry is not the content")
	}
	// The cut read, its retry and the next article: a retry decoded wrong fails
	// its CRC and costs another fetch.
	if n := s.BodyCalls() - calls; n != 3 {
		t.Fatalf("%d BODY commands, want 3", n)
	}
	checkRefsBalanced(t, r)
}

// An article that fails its CRC on every attempt is fetched maxAttempts times,
// not once more by the read that waited on it; and a read that was already served
// bytes of it fails rather than continuing past them.
func TestCorruptArticleFailsTheReadOnce(t *testing.T) {
	cases := map[string]struct {
		bodyOnly   bool // a fetcher without BodyInto: nothing is published, nothing served early
		wantServed bool
	}{
		"served before the CRC failed": {wantServed: true},
		"never served":                 {bodyOnly: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			content := patternBytes(8 * arrivalPart)
			s := nntptest.NewFakeServer(t)
			f := arrivingFile(s, content, arrivalPart)
			r := arrivingReader(t, s, f, false)
			if tc.bodyOnly {
				r.fetcher = struct{ ArticleFetcher }{r.fetcher}
			}
			id := f.Segments[3].MessageID
			n, articles := nntptest.BuildDirectFile("movie.mkv", content, arrivalPart)
			body := bytes.Clone(articles[n.Files[0].Segments[3].MessageID])
			at := len(body) * 3 / 4 // a data line in the part not yet sent when held
			if body[at] == 'A' {
				body[at] = 'B'
			} else {
				body[at] = 'A'
			}
			s.AddArticle(id, body)
			release := s.HoldBody(id)
			defer release()
			calls := s.BodyCalls()

			pos := int64(3*arrivalPart + 1000)
			if _, err := r.Seek(pos, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, 32<<10)
			done := readAllAsync(r, got)
			if tc.wantServed {
				if err := <-done; err != nil || !bytes.Equal(got, content[pos:pos+int64(len(got))]) {
					t.Fatalf("first read: %v, or wrong bytes", err)
				}
				done = readAllAsync(r, make([]byte, arrivalPart))
			}
			time.Sleep(50 * time.Millisecond)
			release()
			err := <-done
			if err == nil {
				t.Fatal("reading through a corrupt article succeeded")
			}
			if served := errors.Is(err, errServedArticleFailed); served != tc.wantServed {
				t.Fatalf("err = %v; want a failure of served bytes: %v", err, tc.wantServed)
			}
			if got := s.BodyCalls() - calls; got != defaultMaxAttempts {
				t.Fatalf("%d BODY commands for the corrupt article, want %d", got, defaultMaxAttempts)
			}
			checkRefsBalanced(t, r)
		})
	}
}

// On a map still estimated from the NZB, the article guessed for a seek may be
// the wrong one: what it publishes must not be served for a position outside it.
func TestSeekOnAnEstimatedMapServesTheRightArticle(t *testing.T) {
	content := patternBytes(8 * arrivalPart)
	s := nntptest.NewFakeServer(t)
	f := arrivingFile(s, content, arrivalPart)
	for i := range f.Segments {
		f.Segments[i].Bytes = arrivalPart * 3 / 4 // understated: seeks are guessed articles late
	}
	r := arrivingReader(t, s, f, true)
	pos := int64(3*arrivalPart + arrivalPart*7/8)
	guess, _, _, _ := r.ix.Locate(pos)
	if guess <= 3 {
		t.Fatalf("the estimate guessed segment %d; the test needs a guess past the real segment 3", guess)
	}
	release := s.HoldBody(f.Segments[guess].MessageID) // the wrong article publishes its front and stops
	defer release()

	if _, err := r.Seek(pos, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 64<<10)
	done := readAllAsync(r, got)
	time.Sleep(100 * time.Millisecond)
	release()
	if err := <-done; err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, content[pos:pos+int64(len(got))]) {
		t.Fatal("seek on an estimated map served the wrong bytes")
	}
	checkRefsBalanced(t, r)
}

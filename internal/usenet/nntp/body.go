package nntp

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
)

// MaxEncodedBodyBytes is the compatibility ceiling for one wire body, including
// CRLF, dot stuffing and terminator. It matches the stream engine's existing
// 16 MiB reservation ceiling; an NZB's untrusted size cannot increase it.
const MaxEncodedBodyBytes = 16 << 20

type BodyTooLargeError struct{ Limit int }

func (e *BodyTooLargeError) Error() string {
	return fmt.Sprintf("nntp: encoded body exceeds %d bytes", e.Limit)
}

// readDotBody receives whole lines into the caller's buffer and reports progress
// after dot unstuffing. EOF without the terminator is an incomplete response.
func readDotBody(r *bufio.Reader, buf []byte, progress BodyProgress) ([]byte, error) {
	return readDotBodyLimit(r, buf, progress, MaxEncodedBodyBytes)
}

func readDotBodyLimit(r *bufio.Reader, buf []byte, progress BodyProgress, limit int) ([]byte, error) {
	if cap(buf) > limit {
		buf = buf[:0:limit]
	}
	b := bodyReceiver{reader: r, out: buf[:0], limit: limit}
	for {
		start := len(b.out)
		if err := b.appendLine(); err != nil {
			return nil, err
		}
		line := bytes.TrimSuffix(b.out[start:], []byte("\n"))
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 1 && line[0] == '.' {
			return b.out[:start], nil
		}
		if len(line) > 0 && line[0] == '.' {
			line = line[1:]
		}
		b.out = append(append(b.out[:start], line...), '\n')
		if progress != nil {
			progress.Line(b.out)
		}
	}
}

type bodyReceiver struct {
	reader          *bufio.Reader
	out             []byte
	received, limit int
}

// Check each fragment before appending, including within unterminated lines.
// Grow explicitly so Go's slice growth cannot reserve beyond the ceiling.
func (b *bodyReceiver) appendLine() error {
	for {
		frag, err := b.reader.ReadSlice('\n')
		if len(frag) > b.limit-b.received {
			return &BodyTooLargeError{Limit: b.limit}
		}
		b.received += len(frag)
		needed := len(b.out) + len(frag)
		if needed > cap(b.out) {
			grown := make([]byte, len(b.out), min(b.limit, max(needed, 2*cap(b.out))))
			copy(grown, b.out)
			b.out = grown
		}
		b.out = append(b.out, frag...)
		if err == io.EOF {
			return io.ErrUnexpectedEOF
		}
		if err != bufio.ErrBufferFull {
			return err
		}
	}
}

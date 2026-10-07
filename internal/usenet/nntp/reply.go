package nntp

import (
	"bytes"
	"errors"
	"fmt"
	"net/textproto"
	"strconv"
)

// MaxReplyBytes bounds one greeting, AUTHINFO or BODY status, including CRLF.
// 64 KiB is a generous compatibility ceiling for metadata, separate from the
// 16 MiB article ceiling. NNTP status is single-line: reject a continuation at
// its first line, so even an unlimited block of small lines cannot accumulate.
const MaxReplyBytes = 64 << 10

type ReplyTooLargeError struct{ Limit int }

func (e *ReplyTooLargeError) Error() string {
	return fmt.Sprintf("nntp: reply exceeds %d bytes", e.Limit)
}

func isReceiveLimit(err error) bool {
	var body *BodyTooLargeError
	var reply *ReplyTooLargeError
	return errors.As(err, &body) || errors.As(err, &reply)
}

// Read from the original buffered reader, preserving any following body bytes.
// Never return a clean alternative status code for incomplete/invalid framing:
// callers may keep a socket after 201, 281, 430 or 423, only for a complete line.
func readReply(tp *textproto.Conn, expect int) (int, string, error) {
	receiver := bodyReceiver{reader: tp.R, limit: MaxReplyBytes}
	if err := receiver.appendLine(); err != nil {
		var oversized *BodyTooLargeError
		if errors.As(err, &oversized) {
			err = &ReplyTooLargeError{Limit: MaxReplyBytes}
		}
		return 0, "", err
	}
	line := bytes.TrimSuffix(receiver.out, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	if len(line) < 4 || line[3] != ' ' {
		return 0, "", textproto.ProtocolError("nntp: expected a complete single-line reply")
	}
	code, err := strconv.Atoi(string(line[:3]))
	if err != nil || code < 100 {
		return 0, "", textproto.ProtocolError("nntp: invalid reply code")
	}
	message := string(line[4:])
	if code != expect {
		return code, message, &textproto.Error{Code: code, Msg: message}
	}
	return code, message, nil
}

package cmd

import (
	"errors"
	"testing"
)

func TestEncoderFailureReport(t *testing.T) {
	exitErr := errors.New("exit status 0xfffffff3")
	for _, tc := range []struct {
		name        string
		err         error
		rejected    bool
		tail        string
		wantCode    string
		wantMessage string
	}{
		{"local failure names the ffmpeg error", exitErr, false, "Permission denied", sessErrTranscodeFailed, "transcode failed: exit status 0xfffffff3: Permission denied"},
		{"no stderr keeps the exit status", exitErr, false, "", sessErrTranscodeFailed, "transcode failed: exit status 0xfffffff3"},
		{"no exit error", nil, false, "", sessErrTranscodeFailed, "transcode produced no output"},
		{"remote refusal is the provider's", exitErr, true, "Server returned 4XX", sessErrSourceUnreachable, "transcode failed: exit status 0xfffffff3: Server returned 4XX"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, message := encoderFailureReport(tc.err, tc.rejected, tc.tail)
			if code != tc.wantCode || message != tc.wantMessage {
				t.Errorf("got (%q, %q), want (%q, %q)", code, message, tc.wantCode, tc.wantMessage)
			}
		})
	}
}

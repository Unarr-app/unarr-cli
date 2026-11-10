package cmd

import "fmt"

// encoderFailureReport builds the session-error report for an encoder that gave
// up: the code the web's watchdog groups by, and a message that names the actual
// ffmpeg error rather than only its exit status.
func encoderFailureReport(exitErr error, sourceRejected bool, stderrTail string) (code, message string) {
	code = sessErrTranscodeFailed
	if sourceRejected {
		code = sessErrSourceUnreachable
	}
	message = "transcode produced no output"
	if exitErr != nil {
		message = fmt.Sprintf("transcode failed: %v", exitErr)
	}
	if stderrTail != "" {
		message += ": " + stderrTail
	}
	return code, message
}

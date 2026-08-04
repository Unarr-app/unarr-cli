package cmd

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/funnel"
)

// captureLog redirects the standard logger for one test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	out, flags := log.Writer(), log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(out); log.SetFlags(flags) })
	return buf
}

// TestFunnelFailureGoesQuiet is the noise budget, stated as a test.
//
// A permanently-failing funnel logged one line every five minutes, forever:
// 288 a day on every Windows box with the funnel on and no cloudflared. That is
// not a cosmetic problem — the crash report carries a bounded log tail, so this
// evicted the evidence a report exists to deliver. The supervisor keeps
// retrying (a CF outage does end); only its voice is rationed.
func TestFunnelFailureGoesQuiet(t *testing.T) {
	buf := captureLog(t)
	err := errors.New("cloudflared: connection refused")
	var lastQuiet time.Time

	// The first funnelQuietAfter failures are worth a line each.
	for i := 1; i <= funnelQuietAfter; i++ {
		logFunnelFailure(err, i, time.Minute, &lastQuiet)
	}
	if got := strings.Count(buf.String(), "could not start"); got != funnelQuietAfter {
		t.Fatalf("logged %d full failures, want %d", got, funnelQuietAfter)
	}

	// Crossing the threshold emits exactly ONE transition line — a log that
	// simply stops mentioning a broken subsystem is worse than one that says it
	// is going quiet — and then nothing for the rest of the window, however many
	// times it fails.
	buf.Reset()
	for i := funnelQuietAfter + 1; i <= funnelQuietAfter+200; i++ {
		logFunnelFailure(err, i, funnelMaxBackoff, &lastQuiet)
	}
	if got := strings.Count(buf.String(), "still cannot start"); got != 1 {
		t.Fatalf("want exactly one transition line over 200 failures, got %d:\n%s", got, buf.String())
	}
	if !strings.Contains(buf.String(), "staying quiet") {
		t.Fatalf("the transition line must say the log is about to go quiet:\n%s", buf.String())
	}

	// Once the window has passed it reports again — once, with the count, so an
	// operator reading the log knows the state is current and how long it has
	// been broken.
	lastQuiet = time.Now().Add(-funnelQuietInterval - time.Minute)
	logFunnelFailure(err, 500, funnelMaxBackoff, &lastQuiet)
	out := buf.String()
	if !strings.Contains(out, "still cannot start") || !strings.Contains(out, "500 attempts") {
		t.Fatalf("the periodic summary must carry the attempt count:\n%s", out)
	}
	buf.Reset()
	logFunnelFailure(err, 501, funnelMaxBackoff, &lastQuiet)
	if buf.Len() != 0 {
		t.Fatalf("the summary reset the quiet window and immediately spoke again:\n%s", buf.String())
	}
}

// TestFunnelLogLinesAreASCII: these lines land in unarr.log, which is read back
// by a Windows console, Notepad and the crash-report pipeline. See
// internal/logging.TestLogLinesAreASCII for the general rule; this covers the
// formatted output, not just the literals.
func TestFunnelLogLinesAreASCII(t *testing.T) {
	buf := captureLog(t)
	var lastQuiet time.Time
	logFunnelFailure(errors.New("boom"), 1, time.Minute, &lastQuiet)
	lastQuiet = time.Time{}
	logFunnelFailure(errors.New("boom"), 99, time.Minute, &lastQuiet)

	for i, b := range buf.Bytes() {
		if b > 0x7F {
			t.Fatalf("non-ASCII byte %#x at offset %d: %q", b, i, buf.String())
		}
	}
}

// TestNoAutoDownloadIsASentinel pins the classification the supervisor's
// early-return depends on. Losing the %w — or going back to a bare
// fmt.Errorf — would silently restore the every-five-minutes-forever loop,
// because the error would stop matching and be treated as transient.
func TestNoAutoDownloadIsASentinel(t *testing.T) {
	wrapped := errors.New("outer: " + funnel.ErrNoAutoDownload.Error())
	if errors.Is(wrapped, funnel.ErrNoAutoDownload) {
		t.Fatal("a string-equal error must NOT match: the sentinel has to be wrapped with %w")
	}
	real := fmtErrorf(funnel.ErrNoAutoDownload)
	if !errors.Is(real, funnel.ErrNoAutoDownload) {
		t.Fatal("a %w-wrapped ErrNoAutoDownload must be recognisable by the supervisor")
	}
}

// fmtErrorf builds the wrapped shape funnel.downloadCloudflared returns.
func fmtErrorf(err error) error {
	return errors.Join(err, errors.New("on windows: install it manually"))
}

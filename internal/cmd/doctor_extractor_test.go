package cmd

import (
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/usenet/postprocess"
)

// With nothing installed the check must NOT warn any more.
//
// It used to: before extraction moved in-process, no binary meant a packed
// release was left as raw .rNN parts, and the "!" prefix rendered that as a
// warning telling the user to apt-install unrar. Now the built-in extractor
// handles it, so warning here would send users to install something they do not
// need — and a doctor that cries wolf is a doctor nobody reads.
func TestExtractorCheckResult_NoWarningWhenNothingInstalled(t *testing.T) {
	orig := findExtractor
	findExtractor = func() (postprocess.ExtractorType, string) {
		return postprocess.ExtractorNone, ""
	}
	t.Cleanup(func() { findExtractor = orig })

	msg, err := extractorCheckResult()
	if err != nil {
		t.Fatalf("a missing external extractor is not an error: %v", err)
	}
	// The leading "!" is what renders a line as a warning.
	if strings.HasPrefix(msg, "!") {
		t.Errorf("missing external extractor still renders as a warning: %q", msg)
	}
	// The user must still learn that extraction IS covered, or the line reads
	// as if nothing can unpack anything.
	if !strings.Contains(msg, "built-in") {
		t.Errorf("result does not say extraction is built in: %q", msg)
	}
}

// COUNTERFACTUAL: an installed binary is named, so the test above is not
// passing merely because the check returns the same string regardless.
func TestExtractorCheckResult_NamesTheFallbackWhenInstalled(t *testing.T) {
	orig := findExtractor
	findExtractor = func() (postprocess.ExtractorType, string) {
		return postprocess.ExtractorUnrar, "/usr/bin/unrar"
	}
	t.Cleanup(func() { findExtractor = orig })

	msg, err := extractorCheckResult()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.HasPrefix(msg, "!") {
		t.Errorf("installed extractor rendered as a warning: %q", msg)
	}
	if !strings.Contains(msg, "unrar") {
		t.Errorf("result does not name the extractor found: %q", msg)
	}
	// And it is described as what it now is — a fallback, not the thing doing
	// the work.
	if !strings.Contains(msg, "fallback") {
		t.Errorf("external extractor not described as a fallback: %q", msg)
	}
}

// The extractor is needed by BOTH download methods, so unlike par2 this check
// must never report "not needed" — a torrent-only user unpacks scene releases
// too. This pins the difference from par2CheckResult, which IS usenet-gated.
func TestExtractorCheckResult_IsNotUsenetGated(t *testing.T) {
	orig := findExtractor
	findExtractor = func() (postprocess.ExtractorType, string) {
		return postprocess.ExtractorNone, ""
	}
	t.Cleanup(func() { findExtractor = orig })

	// No config, no account features consulted at all: the check takes no
	// arguments precisely so it cannot be made conditional by accident.
	msg, err := extractorCheckResult()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(msg, "not needed") {
		t.Errorf("extractor reported as not needed: %q", msg)
	}
}

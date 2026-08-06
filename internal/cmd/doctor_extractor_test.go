package cmd

import (
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/usenet/postprocess"
)

// The extractor check must warn when NOTHING is installed. This is the branch
// that matters and the one a real machine never exercises, so it is stubbed
// rather than skipped.
func TestExtractorCheckResult_WarnsWhenMissing(t *testing.T) {
	orig := findExtractor
	findExtractor = func() (postprocess.ExtractorType, string) {
		return postprocess.ExtractorNone, ""
	}
	t.Cleanup(func() { findExtractor = orig })

	msg, err := extractorCheckResult()
	if err != nil {
		t.Fatalf("a missing extractor is a warning, not an error: %v", err)
	}
	// The leading "!" is what renders the line as a warning instead of a pass;
	// without it the user sees a green tick saying nothing is installed.
	if !strings.HasPrefix(msg, "!") {
		t.Errorf("missing extractor did not render as a warning: %q", msg)
	}
	if !strings.Contains(msg, "unrar") && !strings.Contains(msg, "7z") {
		t.Errorf("warning does not tell the user what to install: %q", msg)
	}
}

// COUNTERFACTUAL: with an extractor present the check passes and names it.
// Without this, the test above would still pass if the check warned
// unconditionally — which would train users to ignore the line.
func TestExtractorCheckResult_PassesWhenInstalled(t *testing.T) {
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

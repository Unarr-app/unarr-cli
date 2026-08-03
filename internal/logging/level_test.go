package logging

import "testing"

func TestParseLevelAcceptsTheSpellingsPeopleType(t *testing.T) {
	cases := map[string]Level{
		"debug":   LevelDebug,
		"TRACE":   LevelDebug,
		" info ":  LevelInfo,
		"warn":    LevelWarn,
		"Warning": LevelWarn,
		"error":   LevelError,
		"err":     LevelError,
		"":        DefaultLevel, // unset is not an error
	}
	for in, want := range cases {
		got, err := ParseLevel(in)
		if err != nil {
			t.Fatalf("ParseLevel(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseLevelRejectsATypo(t *testing.T) {
	if _, err := ParseLevel("warm"); err == nil {
		t.Fatal("a misspelled level must fail loudly, not silently default")
	}
}

func TestLevelStringRoundTrips(t *testing.T) {
	for _, l := range []Level{LevelDebug, LevelInfo, LevelWarn, LevelError} {
		back, err := ParseLevel(l.String())
		if err != nil || back != l {
			t.Fatalf("%v.String() = %q does not parse back (%v, %v)", l, l.String(), back, err)
		}
	}
}

func TestEnabledFiltersBySeverity(t *testing.T) {
	if LevelInfo.Enabled(LevelWarn) {
		t.Fatal("info must be filtered out at --level warn")
	}
	if !LevelError.Enabled(LevelWarn) {
		t.Fatal("error must survive --level warn")
	}
	if !LevelDebug.Enabled(LevelDebug) {
		t.Fatal("a level always survives its own threshold")
	}
}

func TestClassifyReadsTheMarkersTheDaemonActuallyEmits(t *testing.T) {
	cases := map[string]Level{
		"2025/01/20 10:00:00 [usenet] WARNING: par2 not found in PATH": LevelWarn,
		"Error: could not reach the API":                               LevelError,
		"2025/01/20 10:00:00 [acme] generated agent hash abc":          LevelInfo,
		`{"level":"error","message":"boom"}`:                           LevelError,
		"level=warn msg=slow":                                          LevelWarn,
		"[debug] sync tick":                                            LevelDebug,
		"panic: runtime error: index out of range":                     LevelError,
	}
	for line, want := range cases {
		if got := Classify(line); got != want {
			t.Fatalf("Classify(%q) = %v, want %v", line, got, want)
		}
	}
}

func TestClassifyDoesNotPromoteIncidentalProse(t *testing.T) {
	// "errors" appears as a plain word here, not as a severity tag — promoting
	// it would put every happy-path summary in the error bucket.
	if got := Classify("2025/01/20 10:00:00 scan finished with no errors found"); got != LevelInfo {
		t.Fatalf("Classify = %v, want info for a line that merely mentions errors", got)
	}
}

func TestLevelNamesIsACopy(t *testing.T) {
	names := LevelNames()
	names[0] = "mutated"
	if LevelDebug.String() != "debug" {
		t.Fatal("LevelNames must hand out a copy, not the internal table")
	}
}

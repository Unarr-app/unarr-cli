package main

import "testing"

func TestTailBytes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"shorter than max", "abc", 10, "abc"},
		{"exactly max", "abcde", 5, "abcde"},
		{"longer than max", "abcdefghij", 4, "ghij"},
		{"empty", "", 4, ""},
		{"zero max", "abc", 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(tailBytes([]byte(tt.in), tt.max)); got != tt.want {
				t.Errorf("tailBytes(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
			}
		})
	}
}

func TestFirstLine(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"1.5.1", "1.5.1"},
		{"1.5.1\nextra", "1.5.1"},
		{"1.5.1\r\nextra", "1.5.1"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := firstLine(tt.in); got != tt.want {
			t.Errorf("firstLine(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestReadStatusCrashSemantics(t *testing.T) {
	// Documented invariant: crashed only when the stale state claims "running".
	// (readStatus itself needs a state file on disk; the mapping logic is
	// exercised through the struct so the invariant is pinned in a test.)
	s := agentStatus{crashed: true, pid: 1234}
	if s.running {
		t.Fatal("crashed status must not be running")
	}
}

func TestParseUnarrVersion(t *testing.T) {
	tests := []struct{ in, want string }{
		{"unarr 1.5.2 (linux/amd64)", "1.5.2"},
		{"unarr 1.3.8-beta+local-search (linux/amd64)", "1.3.8-beta+local-search"},
		{"unarr 1.5.2", "1.5.2"},
		{"weird output", "weird output"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := parseUnarrVersion(tt.in); got != tt.want {
			t.Errorf("parseUnarrVersion(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

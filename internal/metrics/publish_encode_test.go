package metrics

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func encodeOK(t *testing.T, s Snapshot) Snapshot {
	t.Helper()
	body, err := encodeForPublish(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > MaxPublishSnapshotBytes {
		t.Fatalf("encoded %d bytes, cap is %d", len(body), MaxPublishSnapshotBytes)
	}
	var out Snapshot
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("encoded body is not a snapshot: %v", err)
	}
	return out
}

// More than 50 sessions: the tail is dropped, the head kept in order.
func TestEncodeCapsSessionCount(t *testing.T) {
	var s Snapshot
	for i := 0; i < 80; i++ {
		s.Sessions = append(s.Sessions, session(fmt.Sprintf("s%02d", i)))
	}
	out := encodeOK(t, s)
	if len(out.Sessions) != MaxPublishedSessions {
		t.Fatalf("%d sessions published, want %d", len(out.Sessions), MaxPublishedSessions)
	}
	if out.Sessions[0].ID != "s00" || out.Sessions[49].ID != "s49" {
		t.Fatalf("kept %s..%s, want the head s00..s49", out.Sessions[0].ID, out.Sessions[49].ID)
	}
}

// Under 50 sessions but over 64 KiB: trimmed by the tail until it fits, and as
// many as fit are kept.
func TestEncodeCapsBytes(t *testing.T) {
	var s Snapshot
	for i := 0; i < 50; i++ {
		x := session(fmt.Sprintf("s%02d", i))
		x.ID = strings.Repeat("x", 2000) + x.ID // an oversized id stands in for any fat row
		s.Sessions = append(s.Sessions, x)
	}
	out := encodeOK(t, s)
	if n := len(out.Sessions); n == 0 || n >= 50 {
		t.Fatalf("%d sessions kept, want a trimmed, non-empty list", n)
	}
	// Exactly as many as fit: one more would break the cap.
	more := s
	more.Sessions = s.Sessions[:len(out.Sessions)+1]
	if b, _ := json.Marshal(publicSnapshot(more)); len(b) <= MaxPublishSnapshotBytes {
		t.Fatalf("trimmed to %d sessions although %d fit (%d bytes)", len(out.Sessions), len(more.Sessions), len(b))
	}
}

// The snapshot is the JSON of GET /sessions: same fields, schemaVersion 1, and
// the arrays a consumer relies on are never null.
func TestEncodeKeepsTheContract(t *testing.T) {
	out := encodeOK(t, Snapshot{SchemaVersion: SchemaVersion})
	if out.SchemaVersion != 1 || out.Sessions == nil {
		t.Fatalf("schemaVersion=%d sessions=%v, want 1 and []", out.SchemaVersion, out.Sessions)
	}
}

// Titles come from release and file names; a path or a URL that slipped into
// one must not leave the machine.
func TestEncodeStripsPathsAndURLsFromTitles(t *testing.T) {
	cases := map[string]string{
		"Some.Movie.2160p.mkv":                "Some.Movie.2160p.mkv",
		"AC/DC Live at Donington":             "AC/DC Live at Donington",
		"/home/user/Media/Some.Movie.mkv":     "Some.Movie.mkv",
		`C:\Users\me\Videos\Some.Movie.mkv`:   "Some.Movie.mkv",
		`\\nas\share\Some.Movie.mkv`:          "Some.Movie.mkv",
		"https://cdn.example/f.mkv?token=abc": "",
		"magnet:?xt=urn:btih:abcdef&dn=x":     "",
	}
	for in, want := range cases {
		s := Snapshot{Sessions: []Session{{ID: "a", Kind: KindHLS, Title: in}}}
		if got := encodeOK(t, s).Sessions[0].Title; got != want {
			t.Errorf("title %q published as %q, want %q", in, got, want)
		}
	}
}

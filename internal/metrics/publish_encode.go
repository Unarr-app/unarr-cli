package metrics

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	// MaxPublishedSessions caps the sessions in one published snapshot.
	MaxPublishedSessions = 50
	// MaxPublishSnapshotBytes caps the encoded snapshot. The web refuses a body
	// over 64 KiB; the margin is for the envelope around the snapshot
	// ({"agentId": …, "snapshot": …}).
	MaxPublishSnapshotBytes = 64<<10 - 512
)

// encodeForPublish encodes the snapshot the web receives: titles stripped of
// anything that looks like a path or a URL, at most MaxPublishedSessions
// sessions, and at most MaxPublishSnapshotBytes. Over a cap the tail of the
// session list is dropped (schema v1 has no "truncated" flag; Summary still
// counts every session).
func encodeForPublish(s Snapshot) ([]byte, error) {
	pub := publicSnapshot(s)
	all := pub.Sessions
	pub.Sessions = []Session{}
	head, err := json.Marshal(pub)
	if err != nil {
		return nil, fmt.Errorf("encode snapshot: %w", err)
	}
	// A JSON array is "[" + elements joined by "," + "]", and head already holds
	// the brackets, so the size of every prefix is known without re-encoding.
	budget := MaxPublishSnapshotBytes - len(head)
	if budget < 0 {
		return nil, fmt.Errorf("snapshot without sessions is %d bytes, over the %d cap", len(head), MaxPublishSnapshotBytes)
	}
	n := 0
	for ; n < len(all); n++ {
		row, err := json.Marshal(all[n])
		if err != nil {
			return nil, fmt.Errorf("encode session: %w", err)
		}
		cost := len(row)
		if n > 0 {
			cost++ // the comma
		}
		if cost > budget {
			break
		}
		budget -= cost
	}
	pub.Sessions = all[:n]
	return json.Marshal(pub)
}

// publicSnapshot applies the count cap and the title rule, never mutating s.
func publicSnapshot(s Snapshot) Snapshot {
	n := min(len(s.Sessions), MaxPublishedSessions)
	sessions := make([]Session, n)
	copy(sessions, s.Sessions[:n])
	for i := range sessions {
		sessions[i].Title = publicTitle(sessions[i].Title)
	}
	s.Sessions = sessions
	return s
}

// publicTitle keeps a display title and drops what must not leave the machine:
// a URL (it can carry a token) becomes empty, an absolute path becomes its last
// element. A relative "a/b" is left alone — release names contain slashes.
func publicTitle(t string) string {
	lower := strings.ToLower(t)
	if strings.Contains(t, "://") || strings.HasPrefix(lower, "magnet:") {
		return ""
	}
	if absolutePath(t) {
		return t[strings.LastIndexAny(t, `/\`)+1:]
	}
	return t
}

func absolutePath(t string) bool {
	switch {
	case strings.HasPrefix(t, "/"), strings.HasPrefix(t, `\\`), strings.HasPrefix(t, "~/"):
		return true
	case len(t) >= 3 && t[1] == ':' && (t[2] == '\\' || t[2] == '/'):
		c := t[0] | 0x20
		return c >= 'a' && c <= 'z'
	}
	return false
}

package cmd

import (
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// TestIsUsenetStreamTask locks the routing decision that closes the "instant
// usenet stream hangs" bug: a Mode="stream" task with preferredMethod="usenet" +
// a pre-resolved NzbID (and no InfoHash / no DirectURL) must be recognised as a
// Usenet stream so handleStreamTask routes it to the on-the-fly Usenet streamer
// instead of AddMagnet("") on the torrent path.
func TestIsUsenetStreamTask(t *testing.T) {
	cases := []struct {
		name string
		task agent.Task
		want bool
	}{
		{
			name: "usenet stream: nzbId, no infoHash, no directUrl (the bug case)",
			task: agent.Task{PreferredMethod: "usenet", NzbID: "abc123", Mode: "stream"},
			want: true,
		},
		{
			name: "usenet stream carrying an infoHash (cache key) still routes to usenet",
			task: agent.Task{PreferredMethod: "usenet", NzbID: "abc123", InfoHash: "deadbeef", Mode: "stream"},
			want: true,
		},
		{
			name: "usenet preferred but no nzbId — cannot stream usenet, stays torrent path",
			task: agent.Task{PreferredMethod: "usenet", Mode: "stream"},
			want: false,
		},
		{
			name: "debrid direct-play (directUrl set) is NOT a usenet stream",
			task: agent.Task{PreferredMethod: "usenet", NzbID: "abc123", DirectURL: "https://d/x.mkv", Mode: "stream"},
			want: false,
		},
		{
			name: "torrent stream (infoHash) is not a usenet stream",
			task: agent.Task{PreferredMethod: "torrent", InfoHash: "deadbeef", Mode: "stream"},
			want: false,
		},
		{
			name: "auto preferred with nzbId is not routed as usenet (explicit usenet only)",
			task: agent.Task{PreferredMethod: "auto", NzbID: "abc123", Mode: "stream"},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUsenetStreamTask(tc.task); got != tc.want {
				t.Errorf("isUsenetStreamTask(%+v) = %v, want %v", tc.task, got, tc.want)
			}
		})
	}
}

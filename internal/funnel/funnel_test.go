package funnel

import "testing"

func TestURLPattern(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{
			name: "real quick tunnel banner",
			line: "2026-05-29T22:18:33Z INF |  https://make-appointments-negotiation-blacks.trycloudflare.com  |",
			want: "https://make-appointments-negotiation-blacks.trycloudflare.com",
		},
		{
			name: "two-word hostname",
			line: "https://blue-river.trycloudflare.com is ready",
			want: "https://blue-river.trycloudflare.com",
		},
		{
			name: "control-plane api endpoint is ignored",
			line: `2026-05-29T22:17:59Z DBG POST https://api.trycloudflare.com/tunnel`,
			want: "",
		},
		{
			name: "no trycloudflare url",
			line: "2026-05-29T22:17:44Z INF Requesting new quick Tunnel on trycloudflare.com...",
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := urlPattern.FindString(tc.line); got != tc.want {
				t.Fatalf("FindString(%q) = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

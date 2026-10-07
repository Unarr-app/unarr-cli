package cmd

import (
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

func TestStreamCacheID(t *testing.T) {
	const a = "http://panel.example:8080/movie/user/pass/101.mkv"
	const b = "http://panel.example:8080/movie/user/pass/102.mkv"

	if got := streamCacheID(agent.StreamSession{InfoHash: "abc", DirectURL: a}); got != "abc" {
		t.Fatalf("info_hash must win over the URL, got %q", got)
	}
	idA := streamCacheID(agent.StreamSession{DirectURL: a})
	idB := streamCacheID(agent.StreamSession{DirectURL: b})
	if idA == "" || idB == "" {
		t.Fatalf("a provider URL must yield an identity, got %q / %q", idA, idB)
	}
	if idA == idB {
		t.Fatalf("two different titles share the cache identity %q", idA)
	}
	if idA != streamCacheID(agent.StreamSession{DirectURL: a}) {
		t.Fatal("the identity of one URL must be stable across sessions")
	}
	if strings.Contains(idA, "pass") || strings.Contains(idA, "panel") {
		t.Fatalf("the cache identity leaks the credentialed URL: %q", idA)
	}
	if got := streamCacheID(agent.StreamSession{}); got != "" {
		t.Fatalf("no identity must stay empty (engine then skips the cache), got %q", got)
	}
}

func TestUsenetStreamCacheID(t *testing.T) {
	if got := usenetStreamCacheID(agent.StreamSession{InfoHash: "h", NzbID: "n"}); got != "h" {
		t.Fatalf("got %q", got)
	}
	if got := usenetStreamCacheID(agent.StreamSession{NzbID: "n"}); got != "nzb:n" {
		t.Fatalf("got %q", got)
	}
	if got := usenetStreamCacheID(agent.StreamSession{DirectURL: "http://127.0.0.1:1/x"}); got != "" {
		t.Fatalf("a usenet session must never key on its per-session loopback URL, got %q", got)
	}
}

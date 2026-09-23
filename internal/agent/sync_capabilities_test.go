package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSyncAdvertisesIptvOnlyWhenTheHoldIsWired(t *testing.T) {
	sc, _ := newTestSyncClient("http://unused")
	if caps := sc.buildRequest().Capabilities; len(caps) != 0 {
		t.Fatalf("no IPTV downloader wired: capabilities = %v, want none", caps)
	}
	b, _ := json.Marshal(sc.buildRequest())
	if strings.Contains(string(b), "capabilities") {
		t.Fatalf("an empty capability list must be omitted on the wire: %s", b)
	}

	sc.OnIptvHold = func(bool) {}
	caps := sc.buildRequest().Capabilities
	if len(caps) != 1 || caps[0] != "iptv" {
		t.Fatalf("capabilities = %v, want [iptv]", caps)
	}
}

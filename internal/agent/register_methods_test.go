package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// A daemon on "auto" MUST put preferredMethods on the wire as []. It used to be
// a nil []string with omitempty, so the key vanished, the server left the column
// untouched, and an agent that had once reported ["usenet"] stayed usenet-only
// forever — unfixable from the CLI (the key never comes back) or the web (the
// agent's list beats the web toggle). Restarting the daemon didn't help either.
func TestRegisterRequestSendsExplicitAutoAsEmptyList(t *testing.T) {
	empty := []string{}
	b, err := json.Marshal(RegisterRequest{AgentID: "a", PreferredMethods: &empty})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"preferredMethods":[]`) {
		t.Errorf("auto must serialize as an explicit empty list so the server clears the column; got %s", b)
	}
}

// An explicit order still reaches the server unchanged.
func TestRegisterRequestSendsExplicitList(t *testing.T) {
	list := []string{"debrid", "usenet"}
	b, err := json.Marshal(RegisterRequest{AgentID: "a", PreferredMethods: &list})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"preferredMethods":["debrid","usenet"]`) {
		t.Errorf("explicit list mangled: %s", b)
	}
}

// Partial registrars (`unarr status`, `unarr doctor`) don't read the method
// config, so they must stay silent and leave the stored value alone — sending
// null/[] from them would wipe a preference they know nothing about.
func TestRegisterRequestOmitsMethodsWhenNotReported(t *testing.T) {
	b, err := json.Marshal(RegisterRequest{AgentID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "preferredMethods") {
		t.Errorf("a registrar that doesn't know the preference must omit the key; got %s", b)
	}
}

// Direct-TLS off must be reportable. With omitempty on a plain int/string, 0/""
// vanished and the web kept building URLs to a port the agent had stopped
// listening on, plus a stale hash squatting the unique index.
func TestDirectTLSOffIsReportedExplicitly(t *testing.T) {
	port, hash := directTLSWire(0, "")
	b, err := json.Marshal(RegisterRequest{AgentID: "a", HTTPSStreamPort: port, AgentHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"httpsStreamPort":0`) || !strings.Contains(string(b), `"agentHash":""`) {
		t.Errorf("direct-TLS off must be explicit so the server clears it; got %s", b)
	}
}

func TestDirectTLSOnIsReported(t *testing.T) {
	port, hash := directTLSWire(11819, "abc123abc123abc1")
	b, _ := json.Marshal(SyncRequest{AgentID: "a", HTTPSStreamPort: port, AgentHash: hash})
	if !strings.Contains(string(b), `"httpsStreamPort":11819`) ||
		!strings.Contains(string(b), `"agentHash":"abc123abc123abc1"`) {
		t.Errorf("direct-TLS values mangled: %s", b)
	}
}

// A registrar that doesn't know the direct-TLS state must leave it alone.
func TestDirectTLSOmittedWhenNotReported(t *testing.T) {
	b, _ := json.Marshal(RegisterRequest{AgentID: "a"})
	if strings.Contains(string(b), "httpsStreamPort") || strings.Contains(string(b), "agentHash") {
		t.Errorf("unreported direct-TLS must be omitted, not zeroed; got %s", b)
	}
}

// canDelete is live, not frozen at startup: `unarr config library` + reload has
// to reach the server on the next sync. It used to be read from a config copy
// taken at daemon start, so the web kept saying "file deletion not enabled"
// against a config.toml that said allow_delete = true.
func TestSyncReportsLiveCanDelete(t *testing.T) {
	sc := NewSyncClient(nil, DaemonConfig{CanDelete: false}, NewLocalState())
	if sc.buildRequest().CanDelete {
		t.Fatal("should start false")
	}
	sc.SetCanDelete(true)
	if !sc.buildRequest().CanDelete {
		t.Error("SetCanDelete(true) must be reflected in the next sync request")
	}
	sc.SetCanDelete(false)
	if sc.buildRequest().CanDelete {
		t.Error("turning allow_delete off must stop advertising the capability")
	}
}

// canDelete has no omitempty: false must reach the server so it revokes the
// Delete button, rather than being elided and leaving a stale true.
func TestSyncCanDeleteFalseIsSerialized(t *testing.T) {
	b, err := json.Marshal(SyncRequest{AgentID: "a", CanDelete: false})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"canDelete":false`) {
		t.Errorf("canDelete:false must be explicit on the wire; got %s", b)
	}
}

package agent

import (
	"encoding/json"
	"reflect"
	"testing"
)

// I1: one sync can carry the IPTV hold, closed sessions and a new IPTV stream
// session. The hold must be applied before any stream session starts (so the
// stream never opens the provider while a download still holds the account's
// one connection), and a closed id is torn down before new sessions start.
func TestSyncClient_ProcessResponse_HoldThenClosedThenNewSessions(t *testing.T) {
	sc, _ := newTestSyncClient("http://localhost")
	var order []string
	sc.OnIptvHold = func(held bool) {
		if held {
			order = append(order, "hold:on")
		} else {
			order = append(order, "hold:off")
		}
	}
	sc.OnStreamSessionsClosed = func(ids []string) { order = append(order, "closed:"+ids[0]) }
	sc.OnStreamSession = func(s StreamSession) { order = append(order, "start:"+s.SessionID) }

	var resp SyncResponse
	body := `{"iptvHold":true,"closedStreamSessions":["old"],"streamSessions":[{"sessionId":"new","directUrl":"http://p/movie/u/p/1.mkv","playMethod":"hls","singleConnection":true}]}`
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.StreamSessions[0].SingleConnection {
		t.Fatal("singleConnection did not decode from the web's payload")
	}
	sc.processResponse(&resp)

	want := []string{"hold:on", "closed:old", "start:new"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("callback order = %v, want %v", order, want)
	}
}

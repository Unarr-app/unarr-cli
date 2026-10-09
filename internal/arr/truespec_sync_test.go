package arr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	hashA = "25ceb898aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hashB = "b415d702bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func intp(v int) *int { return &v }

func TestInternalFlagDiffersPerApp(t *testing.T) {
	if InternalFlag("sonarr") != 8 || InternalFlag("radarr") != 32 || InternalFlag("prowlarr") != 0 {
		t.Fatal("Internal flag must be 8 (Sonarr), 32 (Radarr), 0 (Prowlarr)")
	}
}

func TestPlanFlagChanges(t *testing.T) {
	cases := []struct {
		name  string
		app   string
		flags int
		state *TrueSpecState
		from  string
		want  int // -1 = no change
	}{
		{"verified gets Internal (sonarr)", "sonarr", 0, &TrueSpecState{Verified: true}, "Other", 8},
		{"verified gets Internal (radarr)", "radarr", 0, &TrueSpecState{Verified: true}, "Other", 32},
		{"keeps unrelated bits", "sonarr", 16, &TrueSpecState{Verified: true}, "Other", 24},
		{"already set is a no-op", "sonarr", 8, &TrueSpecState{Verified: true}, "Other", -1},
		{"old feed freeleech cleared on TC grab", "sonarr", 1, &TrueSpecState{Verified: true}, "TorrentClaw (Prowlarr)", 8},
		{"freeleech kept on foreign grab", "sonarr", 1, &TrueSpecState{Verified: true}, "Other", 9},
		{"unverified TC grab loses Internal", "sonarr", 9, &TrueSpecState{}, "torrentclaw", 0},
		{"unverified foreign grab keeps Internal", "sonarr", 8, &TrueSpecState{}, "PrivateTracker", -1},
		{"unknown hash untouched", "sonarr", 1, nil, "torrentclaw", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := FlagInputs{
				App:       tc.app,
				Files:     []MediaFile{{ID: 1, IndexerFlags: intp(tc.flags)}},
				HashOf:    map[int]string{1: hashA},
				IndexerOf: map[string]string{hashA: tc.from},
				TrueSpec:  map[string]TrueSpecState{},
				TCMatch:   "torrentclaw",
			}
			if tc.state != nil {
				in.TrueSpec[hashA] = *tc.state
			}
			got := PlanFlagChanges(in)
			if tc.want == -1 {
				if len(got) != 0 {
					t.Fatalf("expected no change, got %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].Flags != tc.want {
				t.Fatalf("flags = %+v, want %d", got, tc.want)
			}
		})
	}
}

func TestPlanSkipsFilesWithoutExposedFlags(t *testing.T) {
	got := PlanFlagChanges(FlagInputs{
		App: "sonarr", Files: []MediaFile{{ID: 1}}, HashOf: map[int]string{1: hashA},
		TrueSpec: map[string]TrueSpecState{hashA: {Verified: true}},
	})
	if len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestLatestImports(t *testing.T) {
	now := time.Now()
	rec := func(file, dl string, ago time.Duration) HistoryRecord {
		return HistoryRecord{DownloadID: dl, Date: now.Add(-ago), Data: HistoryData{FileID: file}}
	}
	got := LatestImports([]HistoryRecord{
		rec("1", strings.ToUpper(hashA), time.Hour),
		rec("1", hashB, 48*time.Hour), // older import of the same file loses
		rec("2", "", time.Hour),       // manual import: no download id
		rec("3", "not-a-hash", time.Hour),
		rec("x", hashA, time.Hour), // bad file id
		rec("4", hashB, 90*24*time.Hour),
	}, now.AddDate(0, 0, -30))
	if len(got) != 1 || got[1] != hashA {
		t.Fatalf("got %v", got)
	}
}

// fakeArr serves a one-file library and records the bulk PUT body.
func fakeArr(t *testing.T, putBody *[]FlagChange, flags int) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/history", func(w http.ResponseWriter, r *http.Request) {
		var recs []map[string]any
		switch r.URL.Query().Get("eventType") {
		case "3":
			recs = []map[string]any{{"eventType": "downloadFolderImported", "downloadId": strings.ToUpper(hashA),
				"date": time.Now().UTC().Format(time.RFC3339), "data": map[string]any{"fileId": "7"}}}
		case "1":
			recs = []map[string]any{{"eventType": "grabbed", "downloadId": strings.ToUpper(hashA),
				"date": time.Now().UTC().Format(time.RFC3339), "data": map[string]any{"indexer": "TorrentClaw"}}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"records": recs})
	})
	mux.HandleFunc("/api/v3/episodefile", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query()["episodeFileIds"]; len(got) != 1 || got[0] != "7" {
			t.Errorf("episodeFileIds = %v", got)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 7, "indexerFlags": flags}})
	})
	mux.HandleFunc("/api/v3/episodefile/bulk", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(putBody)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "k")
}

func verifiedLookup(sent *[][]string) LookupFunc {
	return func(_ context.Context, prefixes []string) (map[string]TrueSpecState, error) {
		*sent = append(*sent, prefixes)
		return map[string]TrueSpecState{hashA: {Verified: true}}, nil
	}
}

func TestSyncWritesInternalFlagAndSendsOnlyPrefixes(t *testing.T) {
	var put []FlagChange
	var sent [][]string
	rep, err := SyncTrueSpecFlags(context.Background(), fakeArr(t, &put, 1), "sonarr", verifiedLookup(&sent),
		SyncOptions{TCMatch: "torrentclaw"})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Applied || len(put) != 1 || put[0].ID != 7 || put[0].Flags != 8 {
		t.Fatalf("report %+v, put %+v", rep, put)
	}
	for _, batch := range sent {
		for _, p := range batch {
			if len(p) != PrefixLen {
				t.Fatalf("prefix %q is not %d chars", p, PrefixLen)
			}
		}
	}
}

func TestSyncDryRunDoesNotWrite(t *testing.T) {
	var put []FlagChange
	var sent [][]string
	rep, err := SyncTrueSpecFlags(context.Background(), fakeArr(t, &put, 0), "sonarr", verifiedLookup(&sent),
		SyncOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Applied || len(put) != 0 || len(rep.Changes) != 1 {
		t.Fatalf("report %+v, put %+v", rep, put)
	}
}

func TestSyncRejectsProwlarr(t *testing.T) {
	if _, err := SyncTrueSpecFlags(context.Background(), NewClient("http://x", "k"), "prowlarr", nil, SyncOptions{}); err == nil {
		t.Fatal("prowlarr has no per-file flags")
	}
}

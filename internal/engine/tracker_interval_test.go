package engine

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

// A tracker that answers interval=0 must not make the announce dispatcher spin.
//
// anacrolix/torrent v1.61.0 schedules the next announce at Completed+Interval with no
// floor, so interval 0 re-announces in a hot loop (~2400/s, one goroutine each). The
// support reports of the dispatcher panics (generics Option.Unwrap "not set", nil
// trigger in indexed.table.runTriggers) show goroutine ids in the millions after hours
// of uptime. third_party/torrent floors the interval; this fails if the replace is
// dropped or the floor removed.
func TestTrackerIntervalZeroDoesNotSpin(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, "d8:intervali0e5:peers0:e")
	}))
	defer srv.Close()

	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = t.TempDir()
	cfg.NoDHT, cfg.DisableUTP, cfg.DisableTCP, cfg.ListenPort = true, true, true, 0
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	const torrents = 3
	for i := range torrents {
		tor, _ := cl.AddTorrentInfoHash(metainfo.Hash{byte(i + 1)})
		tor.AddTrackers([][]string{{srv.URL + "/announce"}})
	}

	time.Sleep(3 * time.Second)
	// One "started" announce per torrent; the floor keeps the follow-ups a minute away.
	if got := hits.Load(); got > torrents {
		t.Fatalf("tracker hit %d times in 3s for %d torrents; interval=0 is spinning the announce dispatcher", got, torrents)
	}
}

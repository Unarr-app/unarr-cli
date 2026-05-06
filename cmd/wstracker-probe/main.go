// wstracker-probe — connects to a WebSocket BitTorrent tracker, advertises
// a fake info_hash, and reports whether the announce succeeds.
//
// Usage:
//
//	go run ./cmd/wstracker-probe -tracker wss://tracker.torrentclaw.com
//
// Exit code 0 on TrackerAnnounceSuccessful, 1 on timeout/error.
package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	alog "github.com/anacrolix/log"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/storage"
	"github.com/pion/webrtc/v4"
)

func main() {
	tracker := flag.String("tracker", "wss://tracker.torrentclaw.com", "WSS tracker URL to probe")
	timeout := flag.Duration("timeout", 30*time.Second, "max wait for successful announce")
	flag.Parse()

	tmp, err := os.MkdirTemp("", "wstracker-probe-*")
	if err != nil {
		log.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(tmp)

	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = tmp
	cfg.DefaultStorage = storage.NewMMap(tmp)
	cfg.Seed = false
	cfg.NoUpload = false
	cfg.DisableTCP = true
	cfg.DisableUTP = true
	cfg.DisableIPv6 = true
	cfg.NoDHT = true
	cfg.NoDefaultPortForwarding = true
	cfg.ListenPort = 0
	cfg.Logger = alog.Default.FilterLevel(alog.Critical)
	cfg.DisableWebtorrent = false
	cfg.ICEServerList = []webrtc.ICEServer{
		{URLs: []string{"stun:stun.l.google.com:19302"}},
	}

	annSuccess := make(chan struct{}, 1)
	annError := make(chan error, 1)
	cfg.Callbacks.StatusUpdated = append(
		cfg.Callbacks.StatusUpdated,
		func(e torrent.StatusUpdatedEvent) {
			switch e.Event { //nolint:exhaustive // peer events are noise for tracker probe
			case torrent.TrackerConnected:
				if e.Error != nil {
					fmt.Printf("[probe] tracker connect FAILED: %v\n", e.Error)
				} else {
					fmt.Printf("[probe] tracker connected: %s\n", e.Url)
				}
			case torrent.TrackerAnnounceSuccessful:
				fmt.Printf("[probe] tracker announce OK: %s ih=%s\n", e.Url, e.InfoHash)
				select {
				case annSuccess <- struct{}{}:
				default:
				}
			case torrent.TrackerAnnounceError:
				fmt.Printf("[probe] tracker announce ERROR: %s ih=%s err=%v\n", e.Url, e.InfoHash, e.Error)
				select {
				case annError <- e.Error:
				default:
				}
			case torrent.TrackerDisconnected:
				fmt.Printf("[probe] tracker disconnected: %s err=%v\n", e.Url, e.Error)
			}
		},
	)

	client, err := torrent.NewClient(cfg)
	if err != nil {
		log.Fatalf("create torrent client: %v", err)
	}
	defer client.Close()

	var ih [20]byte
	if _, err := rand.Read(ih[:]); err != nil {
		log.Fatalf("random info_hash: %v", err)
	}
	magnet := fmt.Sprintf("magnet:?xt=urn:btih:%x&tr=%s", ih, *tracker)
	fmt.Printf("[probe] tracker=%s info_hash=%x timeout=%s\n", *tracker, ih, *timeout)

	t, err := client.AddMagnet(magnet)
	if err != nil {
		log.Fatalf("add magnet: %v", err)
	}
	defer t.Drop()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	select {
	case <-annSuccess:
		fmt.Println("[probe] OK — tracker announce succeeded")
		os.Exit(0)
	case err := <-annError:
		fmt.Printf("[probe] FAIL — tracker announce error: %v\n", err)
		os.Exit(1)
	case <-ctx.Done():
		fmt.Printf("[probe] FAIL — timeout after %s\n", *timeout)
		os.Exit(2)
	}
}

// wstracker-probe — connects to a WebSocket BitTorrent tracker and either
// (a) advertises a fake info_hash to verify announce signalling, or
// (b) seeds a real file via the WebTorrent protocol so a browser
// webtorrent.js client can fetch it for end-to-end verification.
//
// Modes:
//
//	wstracker-probe -tracker wss://tracker.torrentclaw.com
//	    Announces a random info_hash; exits 0 on TrackerAnnounceSuccessful.
//
//	wstracker-probe -tracker wss://… -seed /path/to/file.mp4
//	    Builds a single-file torrent in memory, seeds forever, prints the
//	    magnet (with the WSS tracker injected). Ctrl-C to stop.
//
// Useful for browser ↔ unarr e2e — point a webtorrent.js page at the
// printed magnet and the player should pull pieces via WebRTC data channel.
package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	alog "github.com/anacrolix/log"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"github.com/pion/webrtc/v4"
)

func main() {
	tracker := flag.String("tracker", "wss://tracker.torrentclaw.com", "WSS tracker URL to probe")
	timeout := flag.Duration("timeout", 30*time.Second, "max wait for successful announce (ignored in -seed mode)")
	seedPath := flag.String("seed", "", "path to a file to seed (single-file torrent). When set, runs forever instead of exiting on first announce.")
	flag.Parse()

	if *seedPath != "" {
		runSeeder(*seedPath, *tracker)
		return
	}

	runProbe(*tracker, *timeout)
}

// runProbe — single random-hash announce, exits on success/error/timeout.
func runProbe(trackerURL string, timeout time.Duration) {
	tmp, err := os.MkdirTemp("", "wstracker-probe-*")
	if err != nil {
		log.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(tmp)

	cfg := baseClientConfig(tmp)

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
	magnet := fmt.Sprintf("magnet:?xt=urn:btih:%x&tr=%s", ih, trackerURL)
	fmt.Printf("[probe] tracker=%s info_hash=%x timeout=%s\n", trackerURL, ih, timeout)

	t, err := client.AddMagnet(magnet)
	if err != nil {
		log.Fatalf("add magnet: %v", err)
	}
	defer t.Drop()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	select {
	case <-annSuccess:
		fmt.Println("[probe] OK — tracker announce succeeded")
		os.Exit(0)
	case err := <-annError:
		fmt.Printf("[probe] FAIL — tracker announce error: %v\n", err)
		os.Exit(1)
	case <-ctx.Done():
		fmt.Printf("[probe] FAIL — timeout after %s\n", timeout)
		os.Exit(2)
	}
}

// runSeeder — builds a single-file torrent for the given path, adds it to
// a WebTorrent-enabled client, and seeds until SIGINT/SIGTERM.
func runSeeder(filePath, trackerURL string) {
	abs, err := filepath.Abs(filePath)
	if err != nil {
		log.Fatalf("resolve seed path: %v", err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		log.Fatalf("stat seed file: %v", err)
	}
	if st.IsDir() {
		log.Fatalf("-seed currently supports a single file, not a directory: %s", abs)
	}

	dataDir := filepath.Dir(abs)

	// Build single-file torrent metadata.
	info := metainfo.Info{
		PieceLength: chooseSeedPieceLength(st.Size()),
		Name:        filepath.Base(abs),
	}
	if err := info.BuildFromFilePath(abs); err != nil {
		log.Fatalf("build info from file: %v", err)
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		log.Fatalf("marshal info: %v", err)
	}

	mi := &metainfo.MetaInfo{
		InfoBytes:    infoBytes,
		AnnounceList: metainfo.AnnounceList{{trackerURL}},
		CreatedBy:    "wstracker-probe",
	}
	ih := mi.HashInfoBytes()

	cfg := baseClientConfig(dataDir)
	cfg.Seed = true

	cfg.Callbacks.StatusUpdated = append(
		cfg.Callbacks.StatusUpdated,
		func(e torrent.StatusUpdatedEvent) {
			switch e.Event { //nolint:exhaustive
			case torrent.TrackerConnected:
				if e.Error != nil {
					fmt.Printf("[seed] tracker connect FAILED: %v\n", e.Error)
				} else {
					fmt.Printf("[seed] tracker connected: %s\n", e.Url)
				}
			case torrent.TrackerAnnounceSuccessful:
				fmt.Printf("[seed] tracker announce OK: %s ih=%s\n", e.Url, e.InfoHash)
			case torrent.TrackerAnnounceError:
				fmt.Printf("[seed] tracker announce ERROR: %s err=%v\n", e.Url, e.Error)
			case torrent.TrackerDisconnected:
				fmt.Printf("[seed] tracker disconnected: %s err=%v\n", e.Url, e.Error)
			}
		},
	)

	client, err := torrent.NewClient(cfg)
	if err != nil {
		log.Fatalf("create torrent client: %v", err)
	}
	defer client.Close()

	t, err := client.AddTorrent(mi)
	if err != nil {
		log.Fatalf("add torrent: %v", err)
	}
	t.DownloadAll()

	dn := url.QueryEscape(info.Name)
	enc := url.QueryEscape(trackerURL)
	magnet := fmt.Sprintf("magnet:?xt=urn:btih:%s&dn=%s&tr=%s", ih.HexString(), dn, enc)

	fmt.Printf("[seed] file=%s size=%d bytes piece_length=%d\n", abs, st.Size(), info.PieceLength)
	fmt.Printf("[seed] info_hash=%s\n", ih.HexString())
	fmt.Printf("[seed] magnet=%s\n", magnet)
	fmt.Println("[seed] seeding via WebRTC. Ctrl-C to stop.")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	statTicker := time.NewTicker(5 * time.Second)
	defer statTicker.Stop()

	for {
		select {
		case <-statTicker.C:
			s := t.Stats()
			fmt.Printf("[seed] peers=%d uploaded=%d bytes seeders=%d leechers=%d\n",
				s.ActivePeers, s.BytesWrittenData.Int64(),
				s.ConnectedSeeders, s.ActivePeers-s.ConnectedSeeders)
		case <-stop:
			fmt.Println("[seed] stopping")
			return
		}
	}
}

// baseClientConfig — shared anacrolix client config for both modes.
// WebTorrent is the only transport enabled; TCP/uTP/DHT/IPv6 are disabled
// to keep the moving parts to the minimum required for a WSS-only test.
func baseClientConfig(dataDir string) *torrent.ClientConfig {
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = dataDir
	cfg.DefaultStorage = storage.NewMMap(dataDir)
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
		{URLs: []string{"stun:stun1.l.google.com:19302"}},
	}
	return cfg
}

// chooseSeedPieceLength picks a sane piece size for a given file size.
// Mirrors the libtorrent / qBittorrent ladder so the resulting torrent
// is interoperable with mainstream clients.
func chooseSeedPieceLength(size int64) int64 {
	switch {
	case size < 4*1024*1024: // < 4 MiB
		return 16 * 1024 // 16 KiB
	case size < 64*1024*1024: // < 64 MiB
		return 64 * 1024 // 64 KiB
	case size < 512*1024*1024: // < 512 MiB
		return 256 * 1024 // 256 KiB
	case size < 4*1024*1024*1024: // < 4 GiB
		return 1024 * 1024 // 1 MiB
	default:
		return 4 * 1024 * 1024 // 4 MiB
	}
}
